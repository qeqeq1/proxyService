# proxyService

南京理工大学校外代理服务，供 Web 端跨域访问校内站点。二进制由 GitHub Actions 构建，发布在 [Releases](https://github.com/qeqeq1/proxyService/releases/latest)，部署机不需要装 Go。

## 部署

Linux：

```bash
curl -fsSL https://raw.githubusercontent.com/qeqeq1/proxyService/main/deploy/bootstrap.sh | sudo bash -s -- --origins "https://course.njust.store,https://course-preview.njust.store"
```

Windows（管理员 PowerShell）：

```powershell
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/qeqeq1/proxyService/main/deploy/bootstrap.ps1))) -Origins "https://course.njust.store,https://course-preview.njust.store"
```

`--origins` 是浏览器来源白名单，逗号分隔，默认已填上面两个站点，换域名就改这里。

**留空会怎样**：允许列表为空时，所有带 `Origin` 的请求都会返回 403。浏览器 fetch 一定带 Origin，所以 Web 前端会完全不可用；只有 curl、原生 App 这类不带 Origin 的请求能通过。

## 装完之后

- 后台：`http://127.0.0.1:8081/__admin__`，密码用 `--show-secrets`（Windows `-ShowSecrets`）查看
- 配置：`/etc/njust-proxy/proxy.env`（Windows `C:\ProgramData\njust-proxy\proxy.env`）
- 密钥：同目录的 `secrets.env`。**里面的 `PROXY_STORE_KEY` 丢了，凭据库就解不开**，单独离线备份

升级就是重跑同一条命令；卸载加 `--uninstall`（Windows `-Uninstall`），配置和凭据库会保留。

## 开发

```bash
go test ./...
go run . --host 127.0.0.1 --port 8099 --allowed-targets "*.njust.edu.cn,202.119.0.0/16"
```

## 结构

- `main.go` / `admin.go` / `store.go` / `jar.go` / `limiter.go` / `metrics.go`：代理本体
- `deploy/`：构建与一键部署（Linux systemd / Windows 计划任务）
- `.github/workflows/release.yml`：打 `v*` tag 时构建三平台并发布 Release

接口细节见 `deploy/README.md` 与源码注释。
