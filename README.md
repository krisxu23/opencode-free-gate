# opencode-free-gate

Go 实现的 **OpenCode 免费模型本地网关**：把 opencode.ai 免费模型包装成 OpenAI / Anthropic / Codex 兼容接口，Windows 单文件运行，支持多镜像轮转与多协议代理出口。

```
opencode 客户端
    ↓  http://localhost:13339/openai/v1
opencode-free-gate.exe
    ↓  多镜像轮转 ＋ 并行竞速 ＋ 可选代理出口
opencode.ai/zen ＋ 3 个公共 CDN 镜像
```

## 功能

| 功能 | 说明 |
|---|---|
| **协议兼容** | OpenAI / Anthropic / Codex 三种路由，客户端零改造接入 |
| **并行竞速** | 每次请求同时发往多个出口（手动节点＋池节点＋直连），最快返回者胜出，赢家出现后立即取消其余在途请求 |
| **SSE 保活** | 竞速超过 5 秒未决时自动提前提交 SSE 响应头并发送心跳注释行，客户端不会因等响应头而误判断线 |
| **高级节点** | 内嵌 sing-box v1.13：`vless` `vmess` `trojan` `ss` `hysteria2(hy2)` `tuic` 分享链接直接粘贴，自动转为内部 SOCKS5 端点参与探活和竞速 |
| **在线节点池** | 定时拉取节点源 → 真实请求探活 → 健康入池 / 失效自删；支持文本列表、JSON、**base64 订阅链接** |
| **手动节点保护** | 手动填写的节点无条件保留、永不自动移除 |
| **多镜像轮转** | 上游按请求轮换；连续失败的镜像自动冷却 3 分钟 |
| **模型缓存** | 启动时拉取一次免费模型并长期使用；短名（如 `mimo-v2.5`）自动重定向到 `-free` 模型 |
| **图形界面** | 双击即用；状态 / 设置 / 日志三页，配置存 exe 同目录 `config.json` |

程序不内置任何节点或订阅；不收集任何数据。

## 快速开始

1. 从 [Releases](https://github.com/krisxu23/opencode-free-gate/releases/tag/exe-latest) 下载 `opencode-free-gate-gui.exe`，任意目录双击运行。
2. 编辑 opencode 配置文件（`~/.config/opencode/opencode.jsonc`），添加 provider：

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "freegate": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "baseURL": "http://localhost:13339/openai/v1",
        "apiKey": "sk-local-freegate"
      },
      "models": {
        "mimo-v2.5-free": { "name": "MiMo v2.5" },
        "deepseek-v4-flash-free": { "name": "DeepSeek v4 Flash" }
      }
    }
  }
}
```

3. 在 opencode 里切换到 FreeGate 模型即可。完整模型列表以网关界面为准。

## API 路由

| 协议 | 路由 |
|---|---|
| OpenAI | `/openai/v1/models` · `/openai/v1/chat/completions` |
| Anthropic | `/anthropic/v1/messages` |
| Codex | `/codex/v1/responses` |
| 健康检查 | `/healthz` |

## 节点来源

| 来源 | 用法 |
|---|---|
| 手动节点 | 设置 → 代理节点框粘贴，一行一个；支持 socks5/http URL 与各协议分享链接 |
| 订阅 / 节点源 | 打开「在线节点池」开关并填源链接；支持 base64 订阅、socks5 文本列表、amux JSON、明文分享链接 |
| mihomo 本地 | 填 `socks5://127.0.0.1:7890` 即可复用本机 Clash 的全部节点 |

<details>
<summary>推荐的公共免费代理源（可选，可用率低，仅作备胎）</summary>

```
https://proxy.amux.ai/api/proxies
https://raw.githubusercontent.com/watchttvv/free-proxy-list/refs/heads/main/proxy.txt
https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/protocols/socks5/data.txt
https://bestcf.pages.dev/s5gy/all.txt
https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/socks5.txt
https://raw.githubusercontent.com/roosterkid/openproxylist/main/SOCKS5.txt
https://raw.githubusercontent.com/TheSpeedX/SOCKS-List/master/socks5.txt
```
</details>

<details>
<summary>配合 mihomo 按进程分流（可选）</summary>

```yaml
# proxy-groups 追加
- name: 'OpenCode网关'
  type: select
  proxies: ['OpenCode轮询']
- name: 'OpenCode轮询'
  type: load-balance
  strategy: round-robin
  include-all: true
  exclude-type: 'DIRECT'
  url: 'https://g.cn/generate_204'
  interval: 600

# rules 顶部追加
- PROCESS-NAME,opencode-free-gate.exe,OpenCode网关
```

网关里填 `socks5://127.0.0.1:7890`，切到「走代理」保存重启。
</details>

## 配置

设置在界面修改后「保存并重启」生效，持久化到 exe 同目录 `config.json`（控制台版同样读取该文件）。环境变量优先级更高：

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `PORT` | `13339` | 监听端口 |
| `LISTEN_ADDR` | `127.0.0.1` | 监听地址；设为 `0.0.0.0` 可供局域网设备访问 |
| `CUSTOM_PROXIES` | 空 | 逗号分隔的代理 URL / 分享链接 |
| `MIRROR_URLS` | 空 | 上游镜像基址 |
| `PROXY_LIST_URLS` | 空 | 节点池源链接 |
| `PROXY_RACE` / `PROXY_RACE_WIDTH` | `1` / `8` | 竞速开关 / 自动节点最大并发路数 |
| `PROXY_FIRST_BYTE_TIMEOUT` | `30000` | 流式首字节超时（毫秒） |
| `HARD_TIMEOUT` | `180000` | 流式总预算（毫秒） |
| `STREAM_IDLE_TIMEOUT` | `300000` | 流式开始后上游静默多久算断流（毫秒），长思考模型可调大 |
| `PROXY_ORDER` | 空 | 回退顺序；由 config.json 自动设为 `custom` 或 `direct` |
| `INSECURE_TLS` | `0` | 置 `1` 跳过上游 TLS 证书校验（自签证书的镜像/代理环境用） |

> 默认仅监听本机（127.0.0.1）。上游证书默认严格校验，仅自签证书环境需要 `INSECURE_TLS=1`。
> 「保存并重启」会剔除上表由 config.json 派生的变量后重启进程；`LISTEN_ADDR`、`INSECURE_TLS` 等显式设置的环境变量不受影响。

## 构建

推送后 GitHub Actions 自动构建并发布到 [exe-latest](https://github.com/krisxu23/opencode-free-gate/releases/tag/exe-latest)：`-gui.exe`（图形版）/ `-console.exe`（排障版）。本地构建需 Go 1.24+：

```bash
go test ./...
go build -trimpath -ldflags "-s -w -H windowsgui" -o opencode-free-gate-gui.exe .   # GUI 版先用 go-winres 嵌图标
go build -trimpath -ldflags "-s -w" -o opencode-free-gate-console.exe .
```
