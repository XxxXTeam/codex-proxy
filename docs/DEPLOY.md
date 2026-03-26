# 部署教程

本文说明如何在常见环境下部署 **Codex Proxy**：预编译二进制、Docker、Docker Compose、以及可选的 systemd 与反向代理。

## 前置条件

- 已准备 Codex 账号 JSON（见仓库根目录 [README](../README.md)「快速开始」）。
- 出站网络可访问配置中的后端域名（默认 `chatgpt.com`）；若需代理，在 `config.yaml` 中配置 `proxy-url`。
- 对外暴露服务时，**强烈建议**配置 `api-keys`，并在网关或反向代理上启用 HTTPS。

## 方式一：预编译包（Release）

1. 在 [Releases](https://github.com/XxxXTeam/codex-proxy/releases) 下载对应平台的 zip，并用同目录 `SHA256SUMS.txt` 校验。
2. 解压后复制 `config.yaml`，按 [配置说明](CONFIGURATION.md) 编辑。
3. 将账号 JSON 放入 `auth-dir` 指向的目录（默认 `./auths`）。
4. 启动：
   - Linux / macOS：`./codex-proxy -config config.yaml`
   - Windows：`codex-proxy.exe -config config.yaml`

可选：将 `log-level` 设为 `debug` 排查启动与选号问题，稳定后改回 `info`。

## 方式二：源码编译

```bash
go build -o codex-proxy .
./codex-proxy -config config.yaml
```

需要 **Go 1.25+**（见 `go.mod`）。交叉编译示例：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o codex-proxy .
```

## 方式三：Docker 单容器

镜像由 CI 推送至 GitHub Container Registry，路径与仓库名一致（例如 `ghcr.io/XxxXTeam/codex-proxy`；拉取时若失败可尝试全小写组织名）。

1. 在宿主机准备 `config.yaml` 与 `auths/` 目录。
2. 运行示例：

```bash
docker run -d --name codex-proxy \
  -p 8080:8080 \
  -v /path/to/config.yaml:/app/config.yaml:ro \
  -v /path/to/auths:/app/auths \
  --restart unless-stopped \
  ghcr.io/XxxXTeam/codex-proxy:latest
```

容器内默认命令为 `-config /app/config.yaml`，时区为 `Asia/Shanghai`。若使用数据库模式，请保证容器能访问 `db-host`（勿写 `127.0.0.1` 指宿主机，应使用宿主机 IP 或 Docker 网络中的服务名）。

## 方式四：Docker Compose

仓库根目录 [`docker-compose.yml`](../docker-compose.yml) 提供可选 profile：

- 仅 PostgreSQL：`docker compose --profile postgres up -d`
- 应用 + 数据库（需自备 `config.yaml`，`db-host` 填服务名 `postgres`）：  
  `docker compose --profile postgres --profile app up -d`

可通过同目录 `.env` 覆盖 `APP_PORT`、`POSTGRES_*` 等变量，详见 compose 文件内注释。

## systemd（Linux 服务）

示例单元文件（请按需修改路径与用户）：

```ini
[Unit]
Description=Codex Proxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=codex
WorkingDirectory=/opt/codex-proxy
ExecStart=/opt/codex-proxy/codex-proxy -config /opt/codex-proxy/config.yaml
Restart=on-failure
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now codex-proxy
sudo journalctl -u codex-proxy -f
```

## 反向代理（HTTPS）

代理监听 HTTP（默认 `:8080`）。对外建议使用 **Nginx / Caddy / Traefik** 终止 TLS，并将 `Authorization` 等头原样转发。Nginx 示例片段：

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header Authorization $http_authorization;
    proxy_buffering off;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
}
```

流式（SSE）场景务必关闭不必要的缓冲并放大超时，避免中途断连。

## 健康检查与运维

- 进程存活：`GET /health`
- 账号与额度概览：`GET /stats`（若配置了 `api-keys` 需带 Key）
- 手动刷新 Token：`POST /refresh`；查询额度：`POST /check-quota`（均为管理类接口，行为见 [配置说明](CONFIGURATION.md) 与 `config.example.yaml` 注释）

升级时：先备份 `auths/` 或数据库，替换二进制/镜像后滚动重启即可；配置支持热扫描账号目录（`auth-scan-interval`）。

## 数据库模式简要说明

启用 `db-enabled` 后，账号来自 PostgreSQL / MySQL / SQLite，Token 写回数据库。`db-host` 在 Compose 内应使用服务名（如 `postgres`），在宿主机直连数据库时使用实际地址。导出 JSON 备份可使用：

```bash
./codex-proxy -config config.yaml -tojson
```

详见 [CONFIGURATION.md](CONFIGURATION.md) 数据库相关项。
