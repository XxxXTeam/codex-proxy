# 配置文件说明

配置文件为 **YAML**，默认通过 `-config` 指定路径（默认为 `config.yaml`）。完整注释示例见仓库根目录 `[config.example.yaml](../config.example.yaml)`。

下列默认值以 `internal/config/config.go` 中 `LoadConfig` 初始化为准；若你在 YAML 中显式写出其他值，则以文件为准。

## 核心：监听与后端


| 配置项                       | 默认值           | 说明                                                               |
| ------------------------- | ------------- | ---------------------------------------------------------------- |
| `listen`                  | `:8080`       | 监听地址（`host:port`）。                                               |
| `backend-domain`          | `chatgpt.com` | 后端主机名；未配置 `base-url` 时用于拼出 `https://<domain>/backend-api/codex`。 |
| `backend-resolve-address` | 空             | 可选。将后端解析到指定地址（host 或 `host:port`），用于自定义解析/CNAME。                 |
| `base-url`                | 由 domain 推导   | 若设置则**优先**，并会从 URL 推导 `backend-domain`。需含 `https://`（可省略，代码会补全）。 |


## 账号来源：目录或数据库


| 配置项                                       | 默认值                    | 说明                                                         |
| ----------------------------------------- | ---------------------- | ---------------------------------------------------------- |
| `auth-dir`                                | `./auths`              | 账号 JSON 目录（`db-enabled: false` 时有效）。                       |
| `db-enabled`                              | `false`                | 为 `true` 时从数据库读账号。                                         |
| `db-driver`                               | `postgres`             | `postgres` | `mysql` | `sqlite`（别名 `pg`、`sqlite3` 等会被规范化）。 |
| `db-host`                                 | `127.0.0.1`            | 数据库主机（Compose 内填服务名）。                                      |
| `db-port`                                 | `5432` / `3306`        | 按驱动默认；SQLite 无 TCP 端口。                                     |
| `db-user` / `db-password`                 | 空                      | 数据库凭据。                                                     |
| `db-name`                                 | `codex_proxy`          | 库名；SQLite 无 DSN 时可视为本地文件路径（如 `./data/codex.db`）。           |
| `db-sslmode`                              | `disable`              | PostgreSQL SSL 模式。                                         |
| `db-dsn`                                  | 空                      | 若非空则优先使用驱动原生 DSN。                                          |
| `db-max-open-conns` / `db-max-idle-conns` | `0`                    | `0` 表示按 `refresh-concurrency` 自动估算。                        |
| `db-conn-max-lifetime-sec`                | `0`                     | 连接最大生存时间（秒）；`≤0` 时在 `internal/db/pool.go` 中按 **30 分钟** 生效（SQLite 固定不限制生命周期）。 |
| `accounts`                                | 空                      | 显式账号文件路径列表；不指定则扫描 `auth-dir`。                              |


## 代理与日志


| 配置项         | 默认值    | 说明                                              |
| ----------- | ------ | ----------------------------------------------- |
| `proxy-url` | 空      | 出站 HTTP(S)/SOCKS5 代理，格式见 `config.example.yaml`。 |
| `log-level` | `info` | `debug` / `info` / `warn` / `error`。            |


## Token 刷新与限流策略


| 配置项                                 | 默认值     | 说明                                                                                                          |
| ----------------------------------- | ------- | ----------------------------------------------------------------------------------------------------------- |
| `refresh-interval`                  | `3000`  | 后台自动刷新 Token 间隔（秒）。                                                                                         |
| `refresh-concurrency`               | `50`    | 并发刷新数；账号量极大时可酌情调高。                                                                                          |
| `refresh-batch-size`                | `0`     | `>0` 时分批刷新，降低峰值内存。                                                                                          |
| `refresh-single-timeout-sec`        | `30`    | 单次 OAuth/刷新请求超时（秒）。                                                                                         |
| `refresh-http-429-action`           | 空（内置逻辑） | 刷新遇 HTTP 429：`cooldown` | `remove` | `disable`（简写，等价单阶段策略）。                                                 |
| `quota-http-429-action`             | 空       | 额度接口遇 429 的简写。                                                                                              |
| `quota-http-status-actions`         | 空       | 旧式映射，等价于 `quota-http-status-policy` 的 `phase=none`。                                                         |
| `refresh-http-status-policy`        | 空       | 按状态码：`phase` = `none` | `refresh_once` | `cooldown_then_retry`，`final` = `remove` | `disable` | `cooldown`。 |
| `quota-http-status-policy`          | 空       | 额度查询同上。                                                                                                     |
| `cooldown-401-sec`                  | `30`    | 401 后冷却时间（秒）。                                                                                               |
| `cooldown-429-sec`                  | `60`    | 429 后冷却时间（秒）。                                                                                               |
| `auth-401-sync-refresh-concurrency` | `0`     | 请求路径 401→同步刷新的全局并发；`0` 不限制。                                                                                 |


## 请求重试与选号


| 配置项                    | 默认值           | 说明                                         |
| ---------------------- | ------------- | ------------------------------------------ |
| `max-retry`            | `2`           | 失败换号重试次数（`0` 不重试）；总尝试次数 = `max-retry + 1`。 |
| `enable-healthy-retry` | `true`        | 是否在末几次重试中优先「最近成功」账号。                       |
| `empty-retry-max`      | `2`           | 非流式空结果等场景的额外换号次数。                          |
| `selector`             | `round-robin` | `round-robin` | `quota-first`（优先剩余额度高）。    |


## 健康检查与账号恢复


| 配置项                              | 默认值   | 说明                                      |
| -------------------------------- | ----- | --------------------------------------- |
| `health-check-interval`          | `300` | 间隔（秒）；`0` 关闭定时健康检查。                     |
| `health-check-max-failures`      | `3`   | 连续失败多少次后禁用账号。                           |
| `health-check-concurrency`       | `5`   | 巡检并发（上限 128）。                           |
| `health-check-start-delay`       | `45`  | 启动后延迟开始巡检（秒）。                           |
| `health-check-batch-size`        | `20`  | 每轮最多抽查数量；`0` 表示全量。                      |
| `health-check-request-timeout`   | `8`   | 单次巡检请求超时（秒）。                            |
| `disabled-recovery-interval-sec` | `0`   | 仅磁盘 JSON：周期恢复 `*.json.disabled`；`0` 关闭。 |


## 连接池与 HTTP/2


| 配置项                       | 默认值     | 说明                                                  |
| ------------------------- | ------- | --------------------------------------------------- |
| `max-conns-per-host`      | `12`    | 每主机最大连接数；HTTP/2 过高易触发上游 `GOAWAY ENHANCE_YOUR_CALM`。 |
| `max-idle-conns`          | `48`    | 全局最大空闲连接。                                           |
| `max-idle-conns-per-host` | `8`     | 每主机最大空闲连接。                                          |
| `enable-http2`            | `false` | 出站是否使用 HTTP/2；默认 HTTP/1.1 多连接往往更稳。                  |
| `keepalive-interval`      | `60`    | 上游连接保活 ping 间隔（秒）。                                  |


## 启动、关闭与热加载


| 配置项                           | 默认值            | 说明                       |
| ----------------------------- | -------------- | ------------------------ |
| `startup-async-load`          | `true`         | 先起 HTTP 再后台加载账号。         |
| `startup-load-retry-interval` | `10`           | 后台加载失败重试间隔（秒）。           |
| `startup-load-batch-size`     | `0`（内置默认 8000） | 磁盘每批 JSON 数或 DB 每批行数。    |
| `shutdown-timeout`            | `5`            | 优雅关闭等待时间（秒，限制在 1–60）。    |
| `auth-scan-interval`          | `30`           | 扫描 `auth-dir` 热加载间隔（秒）。  |
| `save-workers`                | `4`            | 异步写回 Token 的工作协程数（1–32）。 |


## 入站 HTTP/2（h2c）


| 配置项                              | 默认值       | 说明                             |
| -------------------------------- | --------- | ------------------------------ |
| `enable-listen-h2c`              | `true`    | 是否对监听启用 HTTP/2 cleartext（h2c）。 |
| `listen-read-header-timeout-sec` | `60`      | 读请求头超时（秒）。                     |
| `listen-idle-timeout-sec`        | `180`     | 空闲超时（秒）；`>0` 时最小约 30。          |
| `listen-tcp-keepalive-sec`       | `30`      | TCP keepalive（秒）。              |
| `listen-max-header-bytes`        | `1048576` | 最大请求头字节数。                      |
| `h2-max-concurrent-streams`      | `1000`    | h2 最大并发流（100–10000）。           |


## 鉴权与额度查询


| 配置项                       | 默认值 | 说明                                                        |
| ------------------------- | --- | --------------------------------------------------------- |
| `api-keys`                | 空   | 非空时，客户端需在 `Authorization: Bearer <key>` 中携带其一；**为空则不校验**。 |
| `quota-check-concurrency` | `0` | `0` 表示使用 `refresh-concurrency`。                           |


## 命令行参数


| 参数               | 说明                                      |
| ---------------- | --------------------------------------- |
| `-config <path>` | 配置文件路径，默认 `config.yaml`。                |
| `-tojson`        | 在已配置数据库的前提下，将库中账号导出为 JSON 到 `auth-dir`。 |


