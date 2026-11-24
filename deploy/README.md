# njust-proxy

二进制由 GitHub Actions 构建，发布在 Releases，部署机不需要装 Go。

## Linux

```bash
sudo ./deploy/deploy.sh --origins "https://你的前端域名"
```

## Windows

```powershell
powershell -ExecutionPolicy Bypass -File .\deploy\deploy.ps1 -Origins "https://你的前端域名"
```

装完会打印两套地址：内网 `http://127.0.0.1:8081/__admin__`，外网 `https://<域名>/__admin__`。部署时加 `--public-host proxy.example.com`（Windows `-PublicHost`）就能让外网地址直接显示你的域名，而不是占位符。密码**首次安装时随机生成**并写进 `secrets.env`，重装不会覆盖；用 `--show-secrets`（Windows `-ShowSecrets`）查看。

## 国内网络

脚本会检测出口地区，在中国大陆时询问是否走镜像；也可以直接用镜像链接，省掉判断：

```bash
# Linux（jsDelivr）
curl -fsSL https://cdn.jsdelivr.net/gh/qeqeq1/proxyService@main/deploy/bootstrap.sh | sudo bash -s -- --origins "https://course.njust.store,https://course-preview.njust.store"
```

```powershell
# Windows
& ([scriptblock]::Create((irm https://cdn.jsdelivr.net/gh/qeqeq1/proxyService@main/deploy/bootstrap.ps1))) -Origins "https://course.njust.store,https://course-preview.njust.store"
```

二进制下载会先试 `https://ghfast.top/https://github.com/...`，失败换 `gh-proxy.com`，最后回官方源。想强制走镜像或官方源：

```bash
PROXY_MIRROR=cn     # 始终镜像
PROXY_MIRROR=github # 始终官方
```

镜像可能有缓存延迟，脚本更新后如果行为没变，用 `PROXY_MIRROR=github` 再跑一次。

`--origins` 默认是 `https://course.njust.store,https://course-preview.njust.store`。它留空时，所有带 `Origin` 的请求都会 403——Web 前端整个不可用，只有 curl/原生 App 能过。

`secrets.env` 里的存储密钥丢了，凭据库就解不开，记得单独备份。

升级就是重跑同一条命令；卸载加 `--uninstall`（Windows `-Uninstall`），配置和凭据库会保留。

## 忘记后台密码

```bash
sudo ./deploy/install.sh --show-secrets
# 或直接读
sudo grep PROXY_ADMIN_PASSWORD /etc/njust-proxy/secrets.env
```

Windows：`install.ps1 -ShowSecrets`，或 `Select-String PROXY_ADMIN_PASSWORD C://ProgramData//njust-proxy//secrets.env`。

重置密码不影响已保存的会话：

```bash
NEW=$(openssl rand -hex 12)
sudo sed -i "s|^PROXY_ADMIN_PASSWORD=.*|PROXY_ADMIN_PASSWORD=${NEW}|" /etc/njust-proxy/secrets.env
sudo systemctl restart njust-proxy
```

**密码可以重置，`PROXY_STORE_KEY` 不行**：密钥一换，旧凭据库全部解不开。`--show-secrets` 会把它一起打出来，注意别外传。
