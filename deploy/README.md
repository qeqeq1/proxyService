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

装完后台在 `http://127.0.0.1:8081/__admin__`，密码用 `--show-secrets`（Windows `-ShowSecrets`）查看。

`secrets.env` 里的存储密钥丢了，凭据库就解不开，记得单独备份。

升级就是重跑同一条命令；卸载加 `--uninstall`（Windows `-Uninstall`），配置和凭据库会保留。
