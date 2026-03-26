# Codex Proxy

[![Go](https://img.shields.io/github/go-mod/go-version/XxxXTeam/codex-proxy?label=Go)](go.mod)
[![License](https://img.shields.io/badge/license-GPL--3.0-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/XxxXTeam/codex-proxy?label=Release)](https://github.com/XxxXTeam/codex-proxy/releases)

Codex API 代理服务，提供 OpenAI / Claude 多协议兼容接口，支持多账号轮询、内部重试、自动 Token 刷新。

## 目录

- [功能](#功能)
- [文档](#文档)
- [快速开始](#快速开始)
- [思考配置](#思考配置)
- [API 接口](#api-接口)
- [配置说明](#配置说明)
- [项目结构](#项目结构)
- [贡献与社区](#贡献与社区)
- [Star 历史](#star-历史)
- [许可证](#许可证)

## 功能

- **多协议兼容** — 同时支持 Chat Completions、Responses API、Responses Compact、Claude Messages API
- **多账号轮询** — Round-Robin + 额度均衡，按使用率排序优先使用剩余额度最多的账号
- **内部重试** — 请求失败时在 executor 内部切换账号重试，流式请求 SSE 头只在成功后才写给客户端，客户端不感知重试过程
- **自动 Token 刷新** — 定期并发刷新 access_token，429 限频设冷却而不是删除账号
- **健康检查** — 定期探测账号状态，自动识别 401/403/429 并处理
- **额度查询** — 支持查询每个账号的剩余额度，按额度使用率排序选号
- **热加载** — 运行时自动扫描新增账号文件，无需重启
- **Tool Schema 自动修复** — 自动补全 `type: array` 缺少 `items` 的 JSON Schema，避免上游 400 错误
- **连接池保活** — 定时 ping 上游保持 TCP+TLS 连接，消除冷启动延迟
- **API Key 鉴权** — 可选的访问密钥保护

## 文档

| 文档 | 说明 |
|------|------|
| [docs/DEPLOY.md](docs/DEPLOY.md) | 部署教程：二进制、Docker、Compose、systemd、反向代理 |
| [docs/CONFIGURATION.md](docs/CONFIGURATION.md) | 配置文件逐项说明（默认值以代码为准） |
| [config.example.yaml](config.example.yaml) | 带注释的完整示例配置 |
| [CONTRIBUTING.md](CONTRIBUTING.md) | 贡献指南与安全披露 |

## 快速开始

### 1. 准备账号文件

在 `auths/` 目录中放入 JSON 格式的账号文件，每个文件一个账号：

```json
{
  "access_token": "eyJhbGciOi...",
  "refresh_token": "v1.MjE0OT...",
  "id_token": "eyJhbGciOi...",
  "account_id": "org-xxx",
  "email": "user@example.com",
  "type": "codex",
  "expired": "2025-01-01T00:00:00Z"
}
```

### 2. 编辑配置

```yaml
listen: ":8080"
auth-dir: "./auths"
base-url: "https://chatgpt.com/backend-api/codex"

# 代理配置（可选）
# 支持 HTTP、HTTPS 和 SOCKS5 代理
# HTTP 代理: http://[user:pass@]host:port
# HTTPS 代理: https://[user:pass@]host:port  
# SOCKS5 代理: socks5://[user:pass@]host:port
# SOCKS5(DNS via proxy): socks5h://[user:pass@]host:port
# proxy-url: "socks5://127.0.0.1:1080"
# proxy-url: "http://user:pass@127.0.0.1:7890"

log-level: "info"
refresh-interval: 3000
max-retry: 2
health-check-interval: 300
health-check-max-failures: 3
health-check-concurrency: 5
refresh-concurrency: 50
api-keys:
  - "sk-your-custom-key"
```

更多字段（数据库、`backend-domain`、HTTP/2、h2c 等）见 [docs/CONFIGURATION.md](docs/CONFIGURATION.md) 与 `config.example.yaml`。

### 3. 预编译包（推荐）


| ZIP 文件名 | 适用环境 |
|------------|----------|
| `codex-proxy-linux-amd64.zip` | Linux x86_64（主流服务器 / PC） |
| `codex-proxy-linux-386.zip` | Linux 32 位 x86 |
| `codex-proxy-linux-arm64.zip` | Linux AArch64（树莓派 64 位、ARM 服务器等） |
| `codex-proxy-linux-armv7.zip` | Linux 32 位 ARMv7（旧版树莓派等） |
| `codex-proxy-windows-amd64.zip` | Windows 64 位 x86 |
| `codex-proxy-windows-386.zip` | Windows 32 位 x86 |
| `codex-proxy-windows-arm64.zip` | Windows ARM64（Surface Pro X 等） |
| `codex-proxy-darwin-amd64.zip` | macOS Intel |
| `codex-proxy-darwin-arm64.zip` | macOS Apple Silicon (M 系列) |

解压后编辑 `config.yaml`，将账号 JSON 放入 `auth-dir` 指定目录，执行：

- Linux / macOS: `./codex-proxy -config config.yaml`
- Windows: `codex-proxy.exe -config config.yaml`

Release 页附带 `SHA256SUMS.txt` 可校验文件完整性。

### 4. 源码编译

```bash
go build -o codex-proxy .
./codex-proxy -config config.yaml
```

### 5. Docker

镜像发布至 `ghcr.io/XxxXTeam/codex-proxy`（amd64 / arm64）。打标签或工作流触发时会推送，详见 [.github/workflows/release.yml](.github/workflows/release.yml)。容器启动与卷挂载的详细步骤见 [docs/DEPLOY.md](docs/DEPLOY.md)。

### 6. 调用接口

**Chat Completions**
```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-custom-key" \
  -d '{"model": "gpt-5.4-high", "messages": [{"role": "user", "content": "Hello!"}], "stream": true}'
```

**Responses API**
```bash
curl http://localhost:8080/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-custom-key" \
  -d '{"model": "gpt-5.4", "input": [{"role": "user", "content": "Hello!"}], "stream": true}'
```

**Claude Messages API**
```bash
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-custom-key" \
  -d '{"model": "gpt-5.4", "max_tokens": 4096, "messages": [{"role": "user", "content": "Hello!"}], "stream": true}'
```

## 思考配置

在模型名中使用连字符后缀控制思考级别；可选 `-fast` 后缀启用快速模式（如 `gpt-5.4-codex-high-fast`）。  
**无后缀模型**（如 `gpt-5`、`gpt-5-codex`）且**客户端未传**思考相关参数（如 `reasoning.effort` / `reasoning_effort`）时，默认**不向请求体写入** `reasoning.effort`，由上游按不传递或 auto 处理。

下表列出各基础模型可用的思考等级与变体，**每个基础模型均可加 `-fast`**（如 `gpt-5-codex-fast`、`gpt-5.1-codex-low-fast`）。

| 基础模型 | 支持的思考等级 | 示例 model id |
|----------|----------------|----------------|
| `gpt-5` | low, medium, high, auto | `gpt-5`、`gpt-5-low`、`gpt-5-high-fast` |
| `gpt-5-codex` | low, medium, high, auto | `gpt-5-codex`、`gpt-5-codex-low-fast`、`gpt-5-codex-auto` |
| `gpt-5-codex-mini` | low, medium, high, auto | `gpt-5-codex-mini`、`gpt-5-codex-mini-medium-fast` |
| `gpt-5.1` | low, medium, high, none, auto | `gpt-5.1`、`gpt-5.1-none-fast`、`gpt-5.1-high` |
| `gpt-5.1-codex` | low, medium, high, max, auto | `gpt-5.1-codex`、`gpt-5.1-codex-max`、`gpt-5.1-codex-max-fast` |
| `gpt-5.1-codex-mini` | low, medium, high, auto | `gpt-5.1-codex-mini-low`、`gpt-5.1-codex-mini-auto-fast` |
| `gpt-5.1-codex-max` | low, medium, high, xhigh, auto | `gpt-5.1-codex-max-low`、`gpt-5.1-codex-max-xhigh-fast` |
| `gpt-5.2` | low, medium, high, xhigh, none, auto | `gpt-5.2`、`gpt-5.2-xhigh-fast`、`gpt-5.2-none` |
| `gpt-5.2-codex` | low, medium, high, xhigh, auto | `gpt-5.2-codex`、`gpt-5.2-codex-xhigh-fast` |
| `gpt-5.3-codex` | low, medium, high, xhigh, none, auto | `gpt-5.3-codex`、`gpt-5.3-codex-none-fast` |
| `gpt-5.4` | low, medium, high, xhigh, none, auto | `gpt-5.4`、`gpt-5.4-xhigh-fast`、`gpt-5.4-auto` |
| `gpt-5.4-mini` | low, medium, high, xhigh, none, auto | `gpt-5.4-mini`、`gpt-5.4-mini-none-fast` |

以上每个 model id 均可再加 `-fast` 得到快速队列变体（如 `gpt-5.4-mini-high-fast`）。

## API 接口

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/chat/completions` | Chat Completions（流式/非流式） |
| POST | `/v1/responses` | Responses API（流式/非流式） |
| POST | `/v1/responses/compact` | Responses Compact API（对话历史压缩） |
| POST | `/v1/messages` | Claude Messages API（流式/非流式） |
| GET | `/v1/models` | 模型列表 |
| GET | `/health` | 健康检查 |
| GET | `/stats` | 账号统计（状态/请求数/错误数/额度） |
| POST | `/recover-auth` | 401 恢复：同步刷新 Token；失败可将凭据重命名为 `*.json.disabled`（见 `config.example.yaml`） |
| POST | `/refresh` | 手动刷新所有账号 Token |
| POST | `/check-quota` | 查询所有账号额度 |

## 配置说明

以下为最常用的几项；**完整键列表、数据库模式、入站 h2c、HTTP 状态策略等**见 [docs/CONFIGURATION.md](docs/CONFIGURATION.md)。默认值以 `internal/config/config.go` 为准。

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `listen` | `:8080` | 监听地址 |
| `auth-dir` | `./auths` | 账号 JSON 目录（非 DB 模式） |
| `backend-domain` | `chatgpt.com` | 后端主机；与 `base-url` 二选一优先逻辑见配置文档 |
| `base-url` | 由 domain 推导 | 显式 Codex API 根 URL |
| `proxy-url` | 空 | 出站 HTTP(S)/SOCKS5 代理 |
| `log-level` | `info` | `debug` / `info` / `warn` / `error` |
| `refresh-interval` | `3000` | Token 自动刷新间隔（秒） |
| `max-retry` | `2` | 失败换号重试次数（总尝试 = `max-retry + 1`） |
| `health-check-interval` | `300` | 健康检查间隔（秒），`0` 关闭 |
| `health-check-max-failures` | `3` | 连续失败多少次后禁用账号 |
| `health-check-concurrency` | `5` | 健康检查并发 |
| `refresh-concurrency` | `50` | Token 并发刷新 |
| `max-conns-per-host` | `12` | 每主机最大连接数；HTTP/2 过高易触发上游 `GOAWAY ENHANCE_YOUR_CALM` |
| `max-idle-conns-per-host` | `8` | 每主机最大空闲连接 |
| `enable-http2` | `false` | 出站 HTTP/2；默认 HTTP/1.1 多连接通常更稳 |
| `enable-listen-h2c` | `true` | 入站是否启用 HTTP/2 cleartext（h2c） |
| `api-keys` | 空 | 非空则校验 `Authorization: Bearer`；空则不鉴权 |

若出现 `http2: server sent GOAWAY ... ENHANCE_YOUR_CALM`，可调低 `max-conns-per-host` / `max-idle-conns-per-host` 或保持 `enable-http2: false`。

## 项目结构

```
codex-proxy/
  |-- main.go                           # 服务入口（入站 HTTP/2 h2c 多路复用）
  |-- config.example.yaml               # 示例配置
  |-- docs/                             # 部署与配置文档
  |-- CONTRIBUTING.md                   # 贡献说明
  |-- auths/                            # 账号文件目录（自建）
  |-- internal/
  |   |-- config/config.go              # 配置加载
  |   |-- static/                       # 嵌入前端（assets/index.html）
  |   |-- auth/                         # 账号、刷新、健康检查、额度
  |   |-- thinking/                     # 思考后缀与协议转换
  |   |-- executor/codex.go             # 上游请求与连接池保活
  |   `-- handler/                      # HTTP 路由、代理、Claude、中间件
  |-- .github/
  |   |-- workflows/release.yml         # 多架构并行打包 + Release + Docker
  |   |-- ISSUE_TEMPLATE/               # Issue 模板
  |   `-- pull_request_template.md      # PR 模板
```

## 贡献与社区

欢迎通过 Issue / PR 参与维护：请先阅读 [CONTRIBUTING.md](CONTRIBUTING.md)（含 **Commit 提交规范**）。提交 Bug 或功能需求时，请使用 Issues 中的 **Bug 报告** 或 **功能建议** 模板；合并请求请按 [Pull Request 模板](.github/pull_request_template.md) 填写自检项。

## Star 历史

[![Star History Chart](https://api.star-history.com/svg?repos=XxxXTeam/codex-proxy&type=Date)](https://star-history.com/#XxxXTeam/codex-proxy&Date)

## 许可证

本项目基于 [GNU General Public License v3.0](LICENSE) 发布。
