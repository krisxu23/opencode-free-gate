package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
)

// advancedBridge 把若干高级协议节点交给内嵌 sing-box：
// 每个节点分配一个 127.0.0.1 本地 SOCKS5 端口，网关其余部分
// （探活、竞速、手动保护）把它们当作普通 socks5 节点使用。
const advancedBasePort = 21000

type advancedItem struct {
	link string // 原始分享链接（作为配置里的身份标识）
	node advNode
}

type advancedBridge struct {
	instance *box.Box
	// localAddr -> 展示名（日志与界面）
	Mapping map[string]string
	// 原始链接 -> 本地地址（配置项还原成可用节点时查询）
	Links map[string]string
}

// startAdvancedBridge 为每个节点启动一个本地 socks 入站。
func startAdvancedBridge(items []advancedItem) (*advancedBridge, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("没有可用的高级协议节点")
	}
	ports, err := allocatePorts(len(items))
	if err != nil {
		return nil, err
	}

	var opts option.Options
	opts.Log = &option.LogOptions{Level: "panic"}
	opts.Route = &option.RouteOptions{}

	bridge := &advancedBridge{
		Mapping: make(map[string]string, len(items)),
		Links:   make(map[string]string, len(items)),
	}
	for i := range items {
		item := &items[i]
		node := &item.node
		inTag := fmt.Sprintf("in-%d", i)
		outTag := fmt.Sprintf("out-%d", i)
		localAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[i])))

		outbound, oerr := buildAdvancedOutbound(outTag, node)
		if oerr != nil {
			return nil, fmt.Errorf("节点 %s: %w", displayNodeName(*node), oerr)
		}
		opts.Inbounds = append(opts.Inbounds, option.Inbound{
			Type: "socks",
			Tag:  inTag,
			SocksInboundOptions: option.SocksInboundOptions{
				ListenOptions: option.ListenOptions{
					Listen:     option.NewListenAddress(netip.MustParseAddr("127.0.0.1")),
					ListenPort: ports[i],
				},
			},
		})
		opts.Outbounds = append(opts.Outbounds, *outbound)
		opts.Route.Rules = append(opts.Route.Rules, option.Rule{
			Type: "default",
			DefaultRule: option.DefaultRule{
				Inbounds:  []string{inTag},
				Outbound:  outTag,
				ClashMode: "",
			},
		})
		display := displayNodeName(*node)
		bridge.Mapping[localAddr] = display
		bridge.Links[item.link] = localAddr
	}

	instance, err := box.New(box.Options{Context: context.Background(), Options: opts})
	if err != nil {
		return nil, fmt.Errorf("sing-box 启动失败: %w", err)
	}
	if err := instance.Start(); err != nil {
		_ = instance.Close()
		return nil, fmt.Errorf("sing-box 启动失败: %w", err)
	}
	bridge.instance = instance
	return bridge, nil
}

func (b *advancedBridge) Close() {
	if b != nil && b.instance != nil {
		_ = b.instance.Close()
	}
}

// buildAdvancedOutbound 按协议生成 sing-box 出站配置。
func buildAdvancedOutbound(tag string, n *advNode) (*option.Outbound, error) {
	out := &option.Outbound{Type: n.kind, Tag: tag}
	switch n.kind {
	case "vless":
		out.VLESSOutboundOptions = option.VLESSOutboundOptions{
			ServerOptions:  option.ServerOptions{Server: n.server, ServerPort: n.port},
			UUID:           n.uuid,
			Flow:           n.flow,
			TLS:            advancedTLS(n, n.tls || n.realityPBK != ""),
			Transport:      advancedTransport(n),
			PacketEncoding: "xudp",
		}
	case "vmess":
		out.VMessOutboundOptions = option.VMessOutboundOptions{
			ServerOptions: option.ServerOptions{Server: n.server, ServerPort: n.port},
			UUID:          n.uuid,
			Security:      "auto",
			TLS:           advancedTLS(n, n.tls),
			Transport:     advancedTransport(n),
		}
	case "trojan":
		out.TrojanOutboundOptions = option.TrojanOutboundOptions{
			ServerOptions: option.ServerOptions{Server: n.server, ServerPort: n.port},
			Password:      n.password,
			TLS:           advancedTLS(n, true),
			Transport:     advancedTransport(n),
		}
	case "ss":
		if n.method == "" || n.password == "" {
			return nil, fmt.Errorf("ss 缺少加密方法或密码")
		}
		out.ShadowsocksOutboundOptions = option.ShadowsocksOutboundOptions{
			ServerOptions: option.ServerOptions{Server: n.server, ServerPort: n.port},
			Method:        n.method,
			Password:      n.password,
		}
	case "hysteria2":
		out.Hysteria2OutboundOptions = option.Hysteria2OutboundOptions{
			ServerOptions: option.ServerOptions{Server: n.server, ServerPort: n.port},
			Password:      n.password,
			TLS:           advancedTLS(n, true),
		}
		if n.obfsPassword != "" {
			out.Hysteria2OutboundOptions.Obfs = &option.Hysteria2Obfs{
				Type:     "salamander",
				Password: n.obfsPassword,
			}
		}
	case "tuic":
		out.TUICOutboundOptions = option.TUICOutboundOptions{
			ServerOptions:     option.ServerOptions{Server: n.server, ServerPort: n.port},
			UUID:              n.uuid,
			Password:          n.password,
			CongestionControl: "bbr",
			ALPN:              splitALPN(n.alpn),
			TLS:               advancedTLS(n, true),
		}
	default:
		return nil, fmt.Errorf("暂不支持的协议 %q", n.kind)
	}
	return out, nil
}

func advancedTLS(n *advNode, enabled bool) *option.OutboundTLSOptions {
	if !enabled {
		return nil
	}
	sni := firstNonEmpty(n.serverName, n.server)
	tls := &option.OutboundTLSOptions{
		Enabled:    true,
		ServerName: sni,
		Insecure:   n.allowInsecure,
		ALPN:       splitALPN(n.alpn),
	}
	if n.realityPBK != "" {
		tls.Reality = &option.OutboundRealityOptions{
			Enabled:  true,
			PublicKey: n.realityPBK,
			ShortID:  n.realitySID,
		}
	} else {
		tls.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: "chrome"}
	}
	return tls
}

func advancedTransport(n *advNode) *option.V2RayTransportOptions {
	switch n.transport {
	case "ws":
		t := &option.V2RayTransportOptions{
			Type:             "ws",
			WebSocketOptions: option.V2RayWebSocketOptions{Path: n.wsPath},
		}
		if n.wsHost != "" {
			t.WebSocketOptions.Headers = map[string]option.Listable[string]{"Host": {n.wsHost}}
		}
		return t
	case "grpc":
		return &option.V2RayTransportOptions{
			Type:        "grpc",
			GRPCOptions: option.V2RayGRPCOptions{ServiceName: n.grpcService},
		}
	case "httpupgrade":
		return &option.V2RayTransportOptions{
			Type:                "httpupgrade",
			HTTPUpgradeOptions:  option.V2RayHTTPUpgradeOptions{Path: n.wsPath, Host: n.wsHost},
		}
	default:
		return nil
	}
}

func splitALPN(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// allocatePorts 从 21000 起挑选 n 个当前空闲的本地端口。
func allocatePorts(count int) ([]uint16, error) {
	ports := make([]uint16, 0, count)
	for candidate := advancedBasePort; candidate < advancedBasePort+10000 && len(ports) < count; candidate++ {
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(candidate)))
		if err != nil {
			continue // 被占用，跳过
		}
		_ = listener.Close()
		ports = append(ports, uint16(candidate))
	}
	if len(ports) < count {
		return nil, fmt.Errorf("本地端口不足")
	}
	return ports, nil
}

func displayNodeName(n advNode) string {
	name := firstNonEmpty(n.tag, fmt.Sprintf("%s:%d", n.server, n.port))
	return fmt.Sprintf("%s (%s)", name, strings.ToUpper(n.kind))
}
