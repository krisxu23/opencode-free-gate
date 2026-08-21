# opencode-free-gate

Go 实现的 **OpenCode 免费模型本地网关**。把 opencode.ai 的免费模型包装成 OpenAI 兼容接口，提供 Windows 图形界面、多镜像自动轮转和可选的代理出口。

```
opencode 客户端
    ↓  http://localhost:13339/openai/v1（OpenAI 兼容）
opencode-free-gate.exe
    ↓  多镜像轮转 + 可选代理出口
opencode.ai/zen ＋ 3 个公共 CDN 镜像
```

## 特性

- **图形界面**：双击即用，无黑窗口；OpenCode 官方图标（文件 / 任务栏 / 标题栏全场景）。
- **实时日志**：倒序显示（最新在最上面），环形缓冲 + 显示层双重上限，长期运行内存占用恒定（约 230KB）。
- **多镜像轮转**：官方上游 + 3 个 CM CDN 镜像按请求轮换；连续失败 2 次的镜像自动冷却 3 分钟。
- **在线节点池**：填入节点源链接并打开开关，后台每轮自动拉取 → 用真实 opencode.ai 请求探活 → 健康节点实时入池、失效自动移除，全程无需重启。
- **模型列表**：每 60 秒从上游刷新免费模型，界面一键复制完整模型 ID（含 `-free` 后缀）；请求里的短名（如 `mimo-v2.5`）自动重定向到对应 `-free` 模型。
- **出站模式**：仅直连 / 自定义代理 二选一；支持 HTTP、HTTPS、SOCKS5 URL 及 `socks://` 分享链接自动转换。
- **配置持久化**：所有设置保存在 exe 同目录的 `config.json`，「保存并重启」立即生效；环境变量优先级高于配置文件。
- **固定 API Key**：`sk-local-freegate`（网关不校验 Key，仅供客户端占位）。

## 快速开始

1. 从 [Releases](https://github.com/krisxu23/opencode-free-gate/releases/tag/exe-latest) 下载 `opencode-free-gate-gui.exe`，放到任意目录双击运行。
2. 编辑 opencode 配置文件（`~/.config/opencode/opencode.jsonc`），添加本地 provider：

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "freegate": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "FreeGate（本地免费网关）",
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

3. 在 opencode 里切换到 FreeGate 的任意模型发消息即可。可用模型以网关界面的「免费模型」区为准。

## API 路由

| 客户端类型 | 路由 |
|---|---|
| OpenAI | `/openai/v1/models`、`/openai/v1/chat/completions` |
| Anthropic | `/anthropic/v1/messages` |
| Codex | `/codex/v1/responses` |
| 健康检查 | `/healthz` |

## 界面说明

| 区域 | 功能 |
|---|---|
| 运行状态 | 监听端口、出站模式、代理在线数；API 地址与默认 Key 一键复制 |
| 免费模型 | 实时拉取的完整模型 ID 列表，支持刷新与一键复制全部 |
| 出站设置 | 直连 / 走代理切换；代理列表编辑（每行一个，支持分享链接） |
| 在线节点池 | 开关 + 节点源链接列表；开启后后台自动拉取、探活、动态入池 |
| 上游镜像 | 每行一个镜像基址，默认预填官方 + 3 个 CM 镜像 |
| 超时设置 | 流式首字节（默认 30s）/ 流式总预算（默认 180s），单位秒 |
| 实时日志 | 倒序滚动，最新日志始终在顶部 |

## 配合 mihomo 使用（可选）

让网关流量经过 mihomo（Clash Meta）节点池，获得更高质量的出口：

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
  lazy: true

# rules 顶部追加
- PROCESS-NAME,opencode-free-gate.exe,OpenCode网关
- DOMAIN-SUFFIX,opencode.ai,OpenCode网关
```

然后在网关界面「出站设置」填入 `socks5://127.0.0.1:7890`（mihomo 默认混合端口），切到「走代理」保存重启。日志出现 `[自定义]` 即生效。

## 配置

### config.json（exe 同目录，界面自动维护）

| 字段 | 默认值 | 说明 |
|---|---|---|
| `port` | `13339` | HTTP 监听端口 |
| `outbound` | `direct` | `direct` 仅直连 / `proxy` 走自定义代理 |
| `proxies` | 空 | 代理 URL 列表 |
| `mirrors` | 官方 + 3 CM 镜像 | 上游镜像基址列表 |
| `first_byte_seconds` | `30` | 流式首字节超时（秒） |
| `budget_seconds` | `180` | 流式总预算（秒） |

### 环境变量（优先级高于 config.json）

| 变量 | 默认值 | 说明 |
|---|---:|---|
| `PORT` | `13339` | 监听端口 |
| `CUSTOM_PROXIES` | 空 | 逗号分隔的代理 URL |
| `MIRROR_URLS` | 空 | 逗号分隔的镜像基址 |
| `PROXY_LIST_URLS` | 空 | 逗号分隔的节点源链接（GUI 开关开启时自动设置） |
| `PROXY_FIRST_BYTE_TIMEOUT` | `30000` | 流式首字节超时（毫秒） |
| `HARD_TIMEOUT` | `10000` | 流式总预算（毫秒） |
| `PROXY_ORDER` | 空 | 回退顺序（`public`/`zen`/`custom`），GUI 模式自动设为 `custom` |

> 环境变量仅在显式设置时生效；GUI 的「保存并重启」不会覆盖已存在的环境变量。

## 构建

推送代码后 GitHub Actions 自动构建并发布两个版本到 [exe-latest](https://github.com/krisxu23/opencode-free-gate/releases/tag/exe-latest)：

- `opencode-free-gate-gui.exe`：图形界面版（嵌入图标与清单）
- `opencode-free-gate-console.exe`：控制台版（日志输出到 stdout，排障用）

本地构建需要 Go 1.24+：

```bash
go test ./...
go run github.com/tc-hib/go-winres@latest simply --icon app.ico --manifest gui --arch amd64   # 仅 GUI 版需要
go build -trimpath -ldflags "-s -w -H windowsgui -X main.uiMode=gui" -o opencode-free-gate-gui.exe .
rm *.syso
go build -trimpath -ldflags "-s -w -X main.uiMode=console" -o opencode-free-gate-console.exe .
```
