package main

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	layerPublic = "public"
	layerZen    = "zen"
	layerCustom = "custom"
	layerDirect = "direct" // 伪层：跳过所有代理层，直接走直连兜底
)

type config struct {
	project          projectSpec
	port             int
	listenAddr       string // 监听地址；默认仅本机，设 0.0.0.0 可供局域网访问
	proxyAPI         string
	proxyMode        string
	proxyOrder       []string
	slotCount        int
	slotRetries      int
	customRetries    int
	zenRetries       int
	customProxies    string
	mirrors          []string
	poolURLs         []string
	zenRelay         string
	zenKey           string
	forceRelay       bool
	gatewayKey       string
	firstByteTimeout time.Duration
	hardTimeout      time.Duration
	nonStreamTimeout time.Duration
	probeTimeout     time.Duration
	refreshInterval  time.Duration
	streamIdle       time.Duration
	raceEnabled      bool // 并行竞速：同一请求同时发往多个出口，最快返回者胜出
	raceWidth        int  // 竞速中自动节点最多同时尝试几路（手动节点始终全上）
}

// upstreamTLSInsecure 由 INSECURE_TLS 控制：置 1 时上游连接跳过证书校验
// （兼容自签证书的镜像/代理环境），默认严格校验。
var upstreamTLSInsecure bool

func loadConfig(project projectSpec) config {
	upstreamTLSInsecure = envIsOn(os.Getenv("INSECURE_TLS"))
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("PROXY_MODE")))
	if mode != "custom" {
		mode = "auto"
	}

	slotCount := envInt("SLOT_COUNT", 5)
	if slotCount < 3 {
		slotCount = 3
	}
	if slotCount > 5 {
		slotCount = 5
	}

	return config{
		project:          project,
		port:             envInt("PORT", 13339),
		listenAddr:       envString("LISTEN_ADDR", "127.0.0.1"),
		proxyAPI:         "https://proxy.amux.ai/api/proxies",
		proxyMode:        mode,
		proxyOrder:       parseProxyOrder(os.Getenv("PROXY_ORDER")),
		slotCount:        slotCount,
		slotRetries:      nonNegative(envInt("SLOT_RETRIES", slotCount)),
		customRetries:    nonNegative(envInt("CUSTOM_RETRIES", 10)),
		zenRetries:       nonNegative(envInt("ZENPROXY_RETRIES", 5)),
		customProxies:    os.Getenv("CUSTOM_PROXIES"),
		mirrors:          parseMirrorEnv(os.Getenv("MIRROR_URLS")),
		poolURLs:         parsePoolSources(os.Getenv("PROXY_LIST_URLS")),
		zenRelay:         envString("ZENPROXY_RELAY", "https://zenproxy.top/api/relay"),
		zenKey:           os.Getenv("ZENPROXY_KEY"),
		forceRelay:       os.Getenv("FORCE_RELAY") == "1",
		gatewayKey:       os.Getenv("GATEWAY_KEY"),
		firstByteTimeout: envMilliseconds("PROXY_FIRST_BYTE_TIMEOUT", 30000),
		hardTimeout:      envMilliseconds("HARD_TIMEOUT", 180000),
		nonStreamTimeout: envMilliseconds("NON_STREAM_TIMEOUT", 300000),
		probeTimeout:     envMilliseconds("PROXY_PROBE_TIMEOUT", 8000),
		refreshInterval:  envMilliseconds("PROXY_REFRESH_MS", 300000),
		streamIdle:       300 * time.Second,
		raceEnabled:      envIsOn(envString("PROXY_RACE", "1")),
		raceWidth:        nonNegative(envInt("PROXY_RACE_WIDTH", 8)),
	}
}

// envIsOn 解析布尔环境变量：1/true/on/y 均视为开启。
func envIsOn(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "on", "y", "yes":
		return true
	default:
		return false
	}
}

// orderedLayers 返回代理层的调度顺序。显式配置 PROXY_ORDER 时优先，
// 否则按 PROXY_MODE 使用原有的固定顺序；直连始终作为最后兜底，不参与排序。
func (c config) orderedLayers() []string {
	if len(c.proxyOrder) > 0 {
		return c.proxyOrder
	}
	if c.proxyMode == "custom" {
		return []string{layerCustom, layerZen}
	}
	return []string{layerPublic, layerZen, layerCustom}
}

func (c config) usesPublicPool() bool {
	for _, layer := range c.orderedLayers() {
		if layer == layerPublic {
			return true
		}
	}
	return false
}

func parseProxyOrder(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	seen := make(map[string]struct{}, 3)
	order := make([]string, 0, 3)
	for _, token := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(token))
		switch name {
		case "public", "s", "slot", "slots":
			name = layerPublic
		case "zen", "zenproxy", "relay":
			name = layerZen
		case "custom":
			name = layerCustom
		case "direct", "none", "off":
			name = layerDirect
		case "":
			continue
		default:
			log.Printf("[配置] PROXY_ORDER 含未知层 %q，已忽略（可用：public、zen、custom、direct）", token)
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		order = append(order, name)
	}
	if len(order) == 0 {
		log.Printf("[配置] PROXY_ORDER=%q 无有效层，回退到 PROXY_MODE 默认顺序", raw)
		return nil
	}
	return order
}

// parseMirrorEnv 解析 MIRROR_URLS 环境变量，无效条目会记录日志后跳过。
func parseMirrorEnv(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	mirrors, bad := parseMirrorList(raw)
	for _, message := range bad {
		log.Printf("[配置] MIRROR_URLS 忽略无效条目: %s", message)
	}
	return mirrors
}

func envString(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		log.Printf("[配置] %s=%q 无效，使用默认值 %d", key, value, fallback)
		return fallback
	}
	return n
}

func envMilliseconds(key string, fallback int) time.Duration {
	n := envInt(key, fallback)
	if n <= 0 {
		n = fallback
	}
	return time.Duration(n) * time.Millisecond
}

func nonNegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}
