# 管理类 HTTP / WebSocket 接口

面向运维与自动化脚本，**与对话 API 共用同一监听端口**。若配置了 `api-keys`，除 `/health` 与根路径 `GET /` 外，下列接口均需在请求头携带：

```http
Authorization: Bearer <与 api-keys 中某一项完全一致>
```

`api-keys` 为空时不校验（仅限可信网络；**公网暴露务必配置密钥**）。

---

## 接口一览

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/stats` | 账号统计（状态、请求数、额度等） |
| POST | `/refresh` | 强制刷新全部账号 Token（SSE 进度流） |
| POST | `/check-quota` | 查询全部账号额度（SSE 进度流） |
| POST | `/recover-auth` | 401 恢复：按邮箱/路径/全部同步刷新；失败可能将凭据重命名为 `*.json.disabled` |
| POST | `/admin/accounts/ingest` | **导入账号**：HTTP 上传 JSON 填充号池（见下文） |
| GET 或 POST | `/admin/accounts/ingest` | 同上路径，带 **`Upgrade: websocket`** 时使用 **WebSocket** 导入（见下文） |
| POST | `/admin/catalog/refresh` | 手动刷新 Codex 客户端模型目录缓存 |

`/stats` 响应同时包含 `catalog` 状态对象：`client_version`、`model_count`、`revision`、`updated_at`、`source`、`refresh_interval_sec`、`last_checked_at` 与最近一次刷新错误。

对话相关接口（`/v1/*`）的鉴权规则相同：配置了 `api-keys` 则必须带 Bearer。

---

## 账号导入：`/admin/accounts/ingest`

用于在不重启服务的情况下向号池追加或更新账号，等价于放入 `auth-dir` 下的 `*.json`（磁盘模式）或写入数据库 `codex_accounts`（`db-enabled: true`）。

### 前置条件

| 模式 | 要求 |
|------|------|
| **仅磁盘**（`db-enabled: false`） | 必须配置 **`auth-dir`**；导入文件会写入该目录。 |
| **数据库**（`db-enabled: true`） | 凭据 upsert 到库；`auth-dir` 可为空。若库中既无 `account_id` 也无 `email`，服务会为该条生成稳定的 `upload_<hash>` 作为 `account_id`。 |

### HTTP：`POST /admin/accounts/ingest`

**Content-Type:** `application/json`（或 `text/plain` 传 NDJSON 亦可，只要 body 字节可被解析）

**请求体**支持三种形式（与磁盘账号文件字段一致，**至少包含 `refresh_token`**）：

1. **单个对象** — 与一个 `*.json` 文件内容相同。  
2. **JSON 数组** — `[{...}, {...}]`，一次提交多个账号。  
3. **NDJSON** — 每行一个 JSON 对象；空行与 `#` 开头行忽略。

**响应** `200` 且 body 为 JSON，例如：

```json
{
  "added": 2,
  "updated": 1,
  "failed": 0,
  "pool_total": 100,
  "errors": []
}
```

| 字段 | 含义 |
|------|------|
| `added` | 新加入号池的账号数 |
| `updated` | 已存在同一逻辑键（磁盘为文件路径，库为 `db:<account_id>` 或 `db:<email>`）时覆盖 Token 的数量 |
| `failed` | 解析或校验失败的对象数 |
| `pool_total` | 导入完成后号池内账号总数 |
| `errors` | 失败条目说明（条数有上限，避免响应过大） |

**400** 时 body 含 `error.message`，多为整段 body 无法解析（例如空 body、JSON 语法错误）。

持久化通过现有 **异步写回队列**完成；与后台刷新相同，极端高并发时若队列已满可能短暂延迟落盘/入库。

**curl 示例（单账号）：**

```bash
curl -sS -X POST "http://127.0.0.1:8080/admin/accounts/ingest" \
  -H "Authorization: Bearer sk-your-custom-key" \
  -H "Content-Type: application/json" \
  -d @./auths/example.json
```

**curl 示例（数组）：**

```bash
curl -sS -X POST "http://127.0.0.1:8080/admin/accounts/ingest" \
  -H "Authorization: Bearer sk-your-custom-key" \
  -H "Content-Type: application/json" \
  -d '[{"refresh_token":"...","email":"a@b.com"}]'
```

### WebSocket：同一 URL

与 `POST /v1/responses` 类似，可使用 **GET 或 POST** 发起握手，并携带：

- `Upgrade: websocket`
- `Connection: Upgrade`  
等标准 WebSocket 头。

连接建立后，**每个文本帧**的 payload 与 **HTTP POST 的 body** 语义相同（单对象、数组或 NDJSON 整段放在一个帧里）。

- 服务端对每一帧返回 **一条** JSON：成功时为与 HTTP 相同的 `IngestResult`；失败时为 `{"ok":false,"error":"..."}`。  
- 发送文本 **`ping`** 可收到 `{"type":"pong"}`（便于探活）。

适合脚本分批推送、或避免单次 HTTP body 过大时拆成多帧（每帧仍应是完整可解析的 JSON 片段，而不是把一个 JSON 对象切成多帧）。

---

## 其他管理接口摘要

### `POST /recover-auth`

请求体示例：`{"email":"a@b.com"}`、`{"file_path":"相对或绝对路径.json"}`、`{"all":true}`。具体行为见 `config.example.yaml` 顶部管理接口注释。

### `POST /refresh`、`POST /check-quota`

响应为 **SSE**（`text/event-stream`），用于进度展示；脚本可按行解析 `data: {...}`。

### `GET /stats`

分页与筛选参数若存在，以当前实现为准（参见 `internal/handler/stats.go`）。

---

## 安全建议

- 导入接口会写入**完整 OAuth 凭据**，权限与修改号池等价；务必 **配置 `api-keys`**、限制来源 IP，或通过反向代理仅对内网开放 `/admin/`。  
- 勿在日志、工单中粘贴真实 `refresh_token`。
