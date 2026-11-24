# Go Proxy

与 Dart 网页代理**协议完全一致**的 Go 反代服务，供 Flutter Web 端绕过浏览器同源策略使用。
只转发到白名单里的学校站点，不配置白名单时拒绝所有代理请求。
协议规范、三份实现的对照与排障见 [`../../docs/Web代理.md`](../../docs/Web代理.md)。

## 接口

| 方法 | 路径 | 响应 |
| --- | --- | --- |
| `GET` | `/__proxy__/health` | `{ ok: true }` |
| `POST` | `/__proxy__/session` | `{ ok, hasActiveLogin }` |
| `POST` | `/__proxy__/request` | `{ statusCode, reasonPhrase, headers, bodyBase64, hasActiveLogin }` |
| `OPTIONS` | 任意 | `204` + CORS 头 |

请求 / 响应信封的字段定义见 `docs/Web代理.md` 第 2 节。

## 配置

| 参数 | 环境变量 | 说明 |
| --- | --- | --- |
| `--host` | | 监听地址，默认 `0.0.0.0` |
| `--port` | | 监听端口，默认 `8081` |
| `--allowed-targets` | `PROXY_ALLOWED_TARGETS` | 目标白名单，逗号分隔，三种写法可混用：精确 `https://bkjw.njust.edu.cn`、后缀 `*.njust.edu.cn`、地址段 `202.119.0.0/16`。为空时拒绝所有代理请求 |
| `--allowed-target-ports` | `PROXY_ALLOWED_TARGET_PORTS` | 后缀 / 地址段命中的目标允许的端口，默认 `80,443,8080,9080`；精确 origin 不受此限制 |
| `--allowed-origins` | `PROXY_ALLOWED_ORIGINS` | 允许跨域调用本服务的网页源，逗号分隔。为空时只接受不带 `Origin` 的请求 |
| `--session-ttl` | `PROXY_SESSION_TTL` | 内存会话空闲上限，默认 `12h` |
| `--credential-ttl` | `PROXY_CREDENTIAL_TTL` | 浏览器会话 Cookie 与凭据库寿命，默认 `1080h`（45 天） |
| `--store-file` | `PROXY_STORE_FILE` | SQLite 凭据库路径，默认 `data/proxy-sessions.db`；留空则不落盘 |
| `--store-key` | `PROXY_STORE_KEY` | 32 字节密钥（base64 或 hex），用于加密 payload；落盘时必填 |
| `--store-plaintext` | `PROXY_STORE_PLAINTEXT=1` | 允许不加密落盘，仅调试用 |
| `--max-sessions` | `PROXY_MAX_SESSIONS` | 内存会话上限，默认 5000，超限淘汰最久未用 |
| `--max-store-entries` | `PROXY_MAX_STORE_ENTRIES` | 凭据库条目上限，默认 20000 |
| `--rate-limit` | `PROXY_RATE_LIMIT_PER_MINUTE` | 每会话每分钟请求数，默认 120；0 关闭 |
| `--max-in-flight` | `PROXY_MAX_IN_FLIGHT` | 全局并发上游请求上限，默认 64；0 关闭 |
| `--admin-user` / `--admin-password` | `PROXY_ADMIN_USER` / `PROXY_ADMIN_PASSWORD` | 后台 Basic Auth；未配置时只允许回环访问 |
| `--trust-proxy-headers` | `PROXY_TRUST_PROXY_HEADERS=1` | 信任 `X-Forwarded-For` / `X-Real-IP`；仅当前面有自己的反代时开启 |

命令行参数优先于环境变量。精确 origin 写完整的 `scheme://host[:port]`，按精确匹配；后缀写法匹配自身及任意子域；地址段写法按 DNS 解析后的 IP 判断，所以裸 IP 目标只有落在段内才放行。

## 部署示例

```bash
export PROXY_ALLOWED_TARGETS="https://bkjw.njust.edu.cn,https://ids.njust.edu.cn,https://ehall2.njust.edu.cn,https://yjs.njust.edu.cn,https://error.njust.edu.cn"
export PROXY_ALLOWED_ORIGINS="https://course.simpleclover.top"
go build -o njust_go_proxy .
./njust_go_proxy --host=127.0.0.1 --port=8081
```

`PROXY_ALLOWED_ORIGINS` 换成网页前端实际的访问地址。如果前端经 `tool/web_server.dart` 同源转发（转发时不带 `Origin`），可以不设：

```bash
dart run tool/web_server.dart --host=0.0.0.0 --port=8080 --proxy-server-base-url=http://127.0.0.1:8081
```

此时浏览器只跟 `:8080` 打交道，`:8081` 不必对外暴露。

本地调试：

```bash
go run . --allowed-targets=https://bkjw.njust.edu.cn,https://ids.njust.edu.cn
```

## 安全行为

- 目标地址和每一跳重定向都要在白名单内，最多跟随 10 次重定向；跳到别的源时丢弃客户端自带的 `Cookie`、`Authorization`、`Host`。
- 建立连接时再解析一次域名，解析到回环、内网（RFC1918 / ULA）、链路本地、CGNAT、组播、未指定地址或 IPv4 映射地址时拒绝。
- 不读取 `HTTP_PROXY` / `HTTPS_PROXY`，直接连接目标站。
- 带 `Origin` 但不在白名单的请求（含 OPTIONS 预检）返回 403，且不返回 `Access-Control-Allow-Origin`。
- 目标站 Cookie 只保存在每个代理会话自己的 Cookie 罐里；`hasActiveLogin` 表示罐里还有未过期、非空的 `JSESSIONID`。
- 代理会话 Cookie 为 `njust_schedule_proxy_session`，`HttpOnly`、`SameSite=None`、HTTPS 下加 `Secure`，并带与 TTL 一致的 `Max-Age`（每次有效请求重新下发，即滑动续期）；会话 ID 为 24 字节随机数的 base64url。非 HTTPS 调试时退化为 `SameSite=Lax`，因为浏览器会丢弃不带 `Secure` 的 `SameSite=None` Cookie。
- 会话空闲 TTL 默认 12 小时，服务端清理和浏览器 Cookie 有效期共用这个值；用 `PROXY_SESSION_TTL`（`time.ParseDuration` 语法，如 `24h`）覆盖。会话仍只存在进程内存，重启即失效。
- 响应头只透传 `content-type` / `location` / `cache-control`；上游请求超时 40 秒。

## 凭据库

内存会话按 `--session-ttl`（默认 12 小时）清理；清理前先把 cookie 写进 SQLite，
按 cookie 自身的过期时间保存（CASTGC 通常 30 天）。用户隔几天回来时，凭 sessionID
直接从库里恢复出同一个罐子，CAS 免密放行，不需要重新输密码。

- 存储是 SQLite（`--store-file`，默认 `data/proxy-sessions.db`，WAL 模式）。
  每行 `payload` 整体 AES-256-GCM 加密，`id` 与时间戳明文（索引用）。
- 密钥 `PROXY_STORE_KEY` 单独注入，不落盘、不进仓库。**没配密钥时默认拒绝启动**，
  避免把 CASTGC 明文摊在磁盘上；调试可显式加 `--store-plaintext`。
- 从旧的整文件 JSON 快照升级时会自动迁移：目标 `.db` 不存在时，会找同目录同名
  `.json`，导入后改名为 `.migrated`。
- 条目上限由 `--max-store-entries` 控制，先按 cookie 过期淘汰，再按最久未用淘汰。
- 后台 `/__admin__` 同时列出内存会话与库里的休眠条目，可以单独清除（彻底删除，
  不会下次又被恢复）。

## 与 CF Workers 版的差异

- 本版错误返回真实的 `4xx` / `5xx`；CF 版一律返回 `200` + `{error}`；
- 本版 `followRedirects` 缺省为 `false`（Go 零值），Dart 本地版缺省为 `true`；
- 本版无请求体大小上限，CF 版为 8 MB；
- 本版会话在进程内存中，重启即失效；CF 版存在 Durable Object 里，可跨实例存活。

## 日志

会打印 `[proxy][session]` / `[proxy][request]` / `[proxy][response]` 三类日志，
包含 `method` 与完整 `url`，**不含请求体与 Cookie**。

## 测试

```bash
go vet ./...
go test -race ./...
```
