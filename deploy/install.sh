#!/usr/bin/env bash
# NJUST go proxy 一键部署（Linux + systemd）
#
#   sudo ./deploy/install.sh                      # 首次部署 / 覆盖升级
#   sudo ./deploy/install.sh --port 8081 --targets "*.njust.edu.cn,202.119.0.0/16"
#   sudo ./deploy/install.sh --binary ./njust-proxy-linux-amd64
#   sudo ./deploy/install.sh --show-secrets        # 再次查看后台账号与密钥
#   sudo ./deploy/install.sh --uninstall
#
# 首次运行会生成：
#   /etc/njust-proxy/proxy.env      可公开的配置（白名单、TTL、限流）
#   /etc/njust-proxy/secrets.env    密钥与后台密码，0600 root，升级时不会覆盖
#   /var/lib/njust-proxy/           SQLite 凭据库（服务用户可写）
set -euo pipefail

APP="njust-proxy"
INSTALL_DIR="/opt/njust-proxy"
CONF_DIR="/etc/njust-proxy"
STATE_DIR="/var/lib/njust-proxy"
UNIT_PATH="/etc/systemd/system/${APP}.service"
RUN_USER="njust-proxy"

PORT="8081"
TARGETS="*.njust.edu.cn,202.119.0.0/16"
ORIGINS=""
ADMIN_USER="admin"
BINARY=""
ACTION="install"
SHOW_SECRETS=0

usage() {
	sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--port) PORT="$2"; shift 2 ;;
	--targets) TARGETS="$2"; shift 2 ;;
	--origins) ORIGINS="$2"; shift 2 ;;
	--admin-user) ADMIN_USER="$2"; shift 2 ;;
	--binary) BINARY="$2"; shift 2 ;;
	--uninstall) ACTION="uninstall"; shift ;;
	--show-secrets) SHOW_SECRETS=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) echo "未知参数：$1（--help 看用法）" >&2; exit 2 ;;
	esac
done

if [ "$(id -u)" -ne 0 ]; then
	echo "请用 root 运行：sudo $0 $*" >&2
	exit 1
fi

random_key() {
	if command -v openssl >/dev/null 2>&1; then
		openssl rand -base64 32 | tr -d '\n'
	else
		head -c 32 /dev/urandom | base64 | tr -d '\n'
	fi
}

random_password() {
	if command -v openssl >/dev/null 2>&1; then
		openssl rand -hex 12
	else
		head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n'
	fi
}

read_env_value() {
	local file="$1" key="$2"
	[ -f "$file" ] || return 0
	sed -n "s/^${key}=//p" "$file" | tail -n1
}

detect_arch() {
	case "$(uname -m)" in
	x86_64 | amd64) echo "amd64" ;;
	aarch64 | arm64) echo "arm64" ;;
	*) echo "不支持的架构：$(uname -m)" >&2; exit 1 ;;
	esac
}

health_check() {
	command -v curl >/dev/null 2>&1 || return 0
	local i
	for i in $(seq 1 20); do
		if curl -fsS --max-time 2 "http://127.0.0.1:${PORT}/__proxy__/health" >/dev/null 2>&1; then
			return 0
		fi
		sleep 0.5
	done
	return 1
}

uninstall() {
	echo "==> 停止并移除服务"
	systemctl disable --now "$APP" 2>/dev/null || true
	rm -f "$UNIT_PATH"
	systemctl daemon-reload
	echo "已移除 unit 与二进制所在目录，配置和凭据库保留："
	echo "  $CONF_DIR  （含密钥与后台密码）"
	echo "  $STATE_DIR （含 SQLite 凭据库）"
	echo "确认不再需要后再手动删除；删除 secrets.env 等于让所有已保存的会话失效。"
}

install_binary() {
	local src="$1"
	if [ ! -f "$src" ]; then
		echo "找不到二进制：$src" >&2
		echo "先在仓库里执行 ./deploy/build.sh，或用 --binary 指定路径。" >&2
		exit 1
	fi
	install -d -m 0755 "$INSTALL_DIR"
	# 先写 .new 再原子替换：正在运行的进程不会被 "Text file busy" 挡住。
	install -m 0755 "$src" "${INSTALL_DIR}/${APP}.new"
	mv -f "${INSTALL_DIR}/${APP}.new" "${INSTALL_DIR}/${APP}"
}

write_unit() {
	cat >"$UNIT_PATH" <<EOF
[Unit]
Description=NJUST go proxy (${APP})
Documentation=https://github.com/Yuriel404/pear_schedule
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${RUN_USER}
Group=${RUN_USER}
EnvironmentFile=${CONF_DIR}/proxy.env
EnvironmentFile=${CONF_DIR}/secrets.env
ExecStart=${INSTALL_DIR}/${APP} --host 127.0.0.1 --port ${PORT}
Restart=on-failure
RestartSec=3
LimitNOFILE=65535

# 最小权限：只允许写自己的数据目录，其余文件系统只读。
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${STATE_DIR}
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
CapabilityBoundingSet=
LockPersonality=true
RestrictNamespaces=true
SystemCallArchitectures=native

[Install]
WantedBy=multi-user.target
EOF
	chmod 0644 "$UNIT_PATH"
}

if [ "$ACTION" = "uninstall" ]; then
	uninstall
	exit 0
fi

if [ "$SHOW_SECRETS" -eq 1 ]; then
	if [ ! -f "${CONF_DIR}/secrets.env" ]; then
		echo "还没有部署过：${CONF_DIR}/secrets.env 不存在" >&2
		exit 1
	fi
	echo "后台账号：${ADMIN_USER}（以 proxy.env 里的 PROXY_ADMIN_USER 为准）"
	echo "后台密码：$(read_env_value "${CONF_DIR}/secrets.env" PROXY_ADMIN_PASSWORD)"
	echo "存储密钥：$(read_env_value "${CONF_DIR}/secrets.env" PROXY_STORE_KEY)"
	echo
	echo "密钥丢失 = 凭据库无法解密，所有用户需要重新登录。请离线备份。"
	exit 0
fi

# 找二进制：--binary > deploy/out/ 下的构建产物 > 当前目录
if [ -z "$BINARY" ]; then
	ARCH="$(detect_arch)"
	for candidate in \
		"$(dirname "$0")/out/${APP}-linux-${ARCH}" \
		"./${APP}-linux-${ARCH}" \
		"./${APP}"; do
		if [ -f "$candidate" ]; then
			BINARY="$candidate"
			break
		fi
	done
fi
if [ -z "$BINARY" ]; then
	echo "没找到 linux-$(detect_arch) 的二进制。" >&2
	echo "先在仓库里执行 ./deploy/build.sh。" >&2
	exit 1
fi

echo "==> 创建运行用户与目录"
if ! id "$RUN_USER" >/dev/null 2>&1; then
	useradd --system --home-dir "$STATE_DIR" --shell /usr/sbin/nologin "$RUN_USER"
fi
install -d -m 0750 -o "$RUN_USER" -g "$RUN_USER" "$STATE_DIR"
install -d -m 0755 "$CONF_DIR"

echo "==> 写入配置"
if [ ! -f "${CONF_DIR}/secrets.env" ]; then
	umask 077
	cat >"${CONF_DIR}/secrets.env" <<EOF
# 只在这里放密钥与密码；升级/改配置不要动这个文件。
PROXY_STORE_KEY=$(random_key)
PROXY_ADMIN_PASSWORD=$(random_password)
EOF
	chmod 0600 "${CONF_DIR}/secrets.env"
	chown root:root "${CONF_DIR}/secrets.env"
	FIRST_INSTALL=1
else
	chmod 0600 "${CONF_DIR}/secrets.env"
	FIRST_INSTALL=0
fi

umask 022
cat >"${CONF_DIR}/proxy.env" <<EOF
# 目标白名单：精确 origin / 域名后缀 / CIDR 三种写法可混用。
PROXY_ALLOWED_TARGETS=${TARGETS}
PROXY_ALLOWED_TARGET_PORTS=80,443,8080,9080
PROXY_ALLOWED_ORIGINS=${ORIGINS}

PROXY_STORE_FILE=${STATE_DIR}/proxy-sessions.db
PROXY_SESSION_TTL=12h
PROXY_CREDENTIAL_TTL=1080h
PROXY_MAX_SESSIONS=5000
PROXY_MAX_STORE_ENTRIES=20000
PROXY_RATE_LIMIT_PER_MINUTE=120
PROXY_MAX_IN_FLIGHT=64
PROXY_ADMIN_USER=${ADMIN_USER}

# 前面有自己的 nginx/Caddy 时打开，否则后台看到的来源 IP 全是 127.0.0.1。
# PROXY_TRUST_PROXY_HEADERS=1
EOF
chmod 0644 "${CONF_DIR}/proxy.env"

echo "==> 安装二进制"
install_binary "$BINARY"

echo "==> 安装 systemd unit"
write_unit
systemctl daemon-reload
systemctl enable "$APP" >/dev/null
systemctl restart "$APP"

echo "==> 健康检查"
if health_check; then
	echo "    http://127.0.0.1:${PORT}/__proxy__/health 正常"
else
	echo "    健康检查未通过，看日志：journalctl -u ${APP} -n 50 --no-pager" >&2
	systemctl --no-pager status "$APP" >&2 || true
	exit 1
fi

ADMIN_PASSWORD="$(read_env_value "${CONF_DIR}/secrets.env" PROXY_ADMIN_PASSWORD)"
cat <<EOF

部署完成。

  服务：systemctl status ${APP}
  日志：journalctl -u ${APP} -f
  后台：http://127.0.0.1:${PORT}/__admin__   （用户名 ${ADMIN_USER}，密码 ${ADMIN_PASSWORD}）
  配置：${CONF_DIR}/proxy.env
  密钥：${CONF_DIR}/secrets.env（0600，root）
  凭据库：${STATE_DIR}/proxy-sessions.db（AES-256-GCM，密钥在 secrets.env）

接下来：
  1. 用 nginx/Caddy 在 443 上反代到 127.0.0.1:${PORT}，并在 proxy.env 里设置
     PROXY_ALLOWED_ORIGINS 与 PROXY_TRUST_PROXY_HEADERS=1，然后重启服务。
  2. 把 secrets.env 里的 PROXY_STORE_KEY 离线备份。丢了这个文件，
     所有已保存的会话都解不开，用户需要重新登录。
  3. 不要把 ${PORT} 直接暴露到公网；当前只监听 127.0.0.1 是刻意的。

EOF

if [ "${FIRST_INSTALL}" -eq 1 ]; then
	echo "注意：上面的后台密码是本次新生成的，之后执行 --show-secrets 可以再次查看。"
fi
