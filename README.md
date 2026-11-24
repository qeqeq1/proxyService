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

## 国内网络

中国大陆网络会自动检测并询问是否走镜像；也可以直接用镜像链接：

```bash
curl -fsSL https://cdn.jsdelivr.net/gh/qeqeq1/proxyService@main/deploy/bootstrap.sh | sudo bash -s -- --origins "https://course.njust.store,https://course-preview.njust.store"
```

二进制会依次尝试 `https://ghfast.top/https://github.com/...`、`gh-proxy.com`、官方源。强制指定：`PROXY_MIRROR=cn` 或 `PROXY_MIRROR=github`。

`--origins` 是浏览器来源白名单，逗号分隔，默认已填上面两个站点，换域名就改这里。

**留空会怎样**：允许列表为空时，所有带 `Origin` 的请求都会返回 403。浏览器 fetch 一定带 Origin，所以 Web 前端会完全不可用；只有 curl、原生 App 这类不带 Origin 的请求能通过。

## 装完之后

- 后台：内网 `http://127.0.0.1:8081/__admin__`，外网 `https://<域名>/__admin__`。部署时加 `--public-host proxy.example.com`（Windows `-PublicHost`），安装提示会直接显示你的域名。密码在首次安装时随机生成，重装不会覆盖；用 `--show-secrets`（Windows `-ShowSecrets`）查看
- 配置：`/etc/njust-proxy/proxy.env`（Windows `C:\ProgramData\njust-proxy\proxy.env`）
- 密钥：同目录的 `secrets.env`。**里面的 `PROXY_STORE_KEY` 丢了，凭据库就解不开**，单独离线备份

升级就是重跑同一条命令；卸载加 `--uninstall`（Windows `-Uninstall`），配置和凭据库会保留。

## 忘记后台密码

密码在 `secrets.env` 里，直接读，或让脚本打印：

```bash
# Linux
sudo ./deploy/install.sh --show-secrets
# 没保留脚本时：
sudo grep PROXY_ADMIN_PASSWORD /etc/njust-proxy/secrets.env
```

Windows（管理员）：`install.ps1 -ShowSecrets`，或

```powershell
Select-String PROXY_ADMIN_PASSWORD C:\ProgramData\njust-proxy\secrets.env
```

`--show-secrets` 会把 `PROXY_STORE_KEY` 一起打印，那个不要外传。

确实找不回就重置密码，不影响已保存的会话：

```bash
NEW=$(openssl rand -hex 12)
sudo sed -i "s|^PROXY_ADMIN_PASSWORD=.*|PROXY_ADMIN_PASSWORD=${NEW}|" /etc/njust-proxy/secrets.env
sudo systemctl restart njust-proxy
echo "新密码：${NEW}"
```

注意：**密码可以重置，存储密钥不行**。`PROXY_STORE_KEY` 一改，旧凭据库全部解不开，所有用户都要重新登录一次。

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
