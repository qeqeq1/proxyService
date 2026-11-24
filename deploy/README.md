# njust-proxy 部署包

这个目录是可独立分发的部署包：一台干净的 Linux 或 Windows 机器，只要有一个二进制和这里的脚本，就能把代理跑起来并配好守护进程。

代理本体是上层的 Go 源码，本目录只放构建与部署相关的东西。

## 一分钟部署

### Linux（systemd）

```bash
# 1. 在仓库根目录交叉编译（本机需要有 Go）
./deploy/build.sh

# 2. 传到服务器，一条命令装好
sudo ./deploy/install.sh --origins "https://course.example.com"
```

安装脚本会：创建 `njust-proxy` 系统用户、生成 32 字节存储密钥和后台密码、写配置、装 systemd unit（含 `ProtectSystem=strict` 等最小权限项）、启动并做健康检查。

只监听 `127.0.0.1:8081`，不直接对外。

### Windows（计划任务）

```powershell
# 1. 构建（在 Git Bash 里跑），或直接用现成的 exe
./deploy/build.sh windows/amd64

# 2. 管理员 PowerShell 里执行
powershell -ExecutionPolicy Bypass -File .\deploy\install.ps1 -Origins "https://course.example.com"
```

安装脚本会：生成密钥与密码、写配置、注册开机启动的计划任务（SYSTEM 身份、失败自动重启）、启动并做健康检查。目录：

```
C:\Program Files\njust-proxy\          二进制
C:\ProgramData\njust-proxy\proxy.env   配置
C:\ProgramData\njust-proxy\secrets.env 密钥与密码（ACL 只给 SYSTEM/Administrators）
C:\ProgramData\njust-proxy\data\       SQLite 凭据库
```

## 常用参数

| 参数 | Linux | Windows | 说明 |
| --- | --- | --- | --- |
| 端口 | `--port 8081` | `-Port 8081` | 只监听回环 |
| 目标白名单 | `--targets "*.njust.edu.cn,202.119.0.0/16"` | `-Targets ...` | 精确 origin / 后缀 / CIDR |
| 浏览器来源 | `--origins "https://a.com"` | `-Origins ...` | 不填则只接受无 Origin 的请求 |
| 后台账号 | `--admin-user admin` | `-AdminUser admin` | |
| 指定二进制 | `--binary ./xxx` | `-Binary .\xxx.exe` | |
| 查看密钥 | `--show-secrets` | `-ShowSecrets` | 打印后台密码与存储密钥 |
| 卸载 | `--uninstall` | `-Uninstall` | 保留配置与凭据库 |

## 密钥与备份

`secrets.env` 里有两项：`PROXY_STORE_KEY`（32 字节，AES-256-GCM 加密凭据库）和 `PROXY_ADMIN_PASSWORD`。

**存储密钥丢了，凭据库就解不开**，所有用户需要重新登录一次。请单独离线备份，不要和数据库放在同一块盘上。升级脚本不会覆盖这个文件。

## 放到 TLS 后面

代理本身只说 HTTP。线上建议在前面加一层反代：

```nginx
server {
    listen 443 ssl;
    server_name proxy.example.com;
    client_max_body_size 8m;

    location / {
        proxy_pass http://127.0.0.1:8081;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_read_timeout 60s;
    }
}
```

然后修改配置，把 `PROXY_TRUST_PROXY_HEADERS=1` 打开（否则后台看到的来源 IP 全是 127.0.0.1），并把 `PROXY_ALLOWED_ORIGINS` 设成前端的实际来源，最后重启服务。

Caddy 等价写法：

```
proxy.example.com {
    reverse_proxy 127.0.0.1:8081
}
```

## 升级

重跑安装脚本就是升级，不需要额外参数：

```bash
./deploy/build.sh                 # 重新构建
sudo ./deploy/install.sh          # 替换二进制、重写 unit、重启
```

已存在的 `secrets.env` 不会被覆盖，配置、密钥、凭据库都保留。

## 卸载

```bash
sudo ./deploy/install.sh --uninstall
```

会移除服务和二进制，保留 `/etc/njust-proxy` 与 `/var/lib/njust-proxy`。确认不需要后再手动删；删 `secrets.env` 等于放弃已保存的会话。

## 本地调试

不想装系统服务时，直接在仓库里跑：

```bash
PROXY_STORE_KEY="$(openssl rand -base64 32)" \
PROXY_STORE_FILE=/tmp/proxy.db \
go run . --host 127.0.0.1 --port 8099 --allowed-targets "*.njust.edu.cn,202.119.0.0/16"
```

后台面板在 `/__admin__`，首次启动的账号密码用 `--admin-user/--admin-password` 指定；不指定时只允许本机访问。
