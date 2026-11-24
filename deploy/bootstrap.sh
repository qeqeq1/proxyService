#!/usr/bin/env bash
# 一键入口：不 clone、不装 Go，下载脚本后直接部署。
#
#   curl -fsSL https://raw.githubusercontent.com/qeqeq1/proxyService/main/deploy/bootstrap.sh | sudo bash -s -- --origins "https://a.example.com"
#
# 中国大陆网络会自动改用镜像；也可以显式指定：
#   PROXY_MIRROR=cn      强制镜像
#   PROXY_MIRROR=github  强制官方源
#   PROXY_MIRROR=auto    默认，按出口地区判断
set -euo pipefail

PROXY_MIRROR="${PROXY_MIRROR:-auto}"
export PROXY_MIRROR

case "$(uname -m)" in
x86_64 | amd64) goarch="amd64" ;;
aarch64 | arm64) goarch="arm64" ;;
*) goarch="amd64" ;;
esac

is_cn_network() {
	command -v curl >/dev/null 2>&1 || return 1
	local country
	country="$(curl -fsS --max-time 3 https://ipinfo.io/country 2>/dev/null | tr -d '[:space:]' | tr '[:lower:]' '[:upper:]' || true)"
	[ "$country" = "CN" ]
}

use_mirror() {
	case "$PROXY_MIRROR" in
	cn) return 0 ;;
	github) return 1 ;;
	esac
	is_cn_network || return 1
	# 只有真终端才询问；curl | bash 时 stdin 是管道，问了也读不到答案。
	if [ -t 0 ] && [ -t 1 ]; then
		local answer
		read -r -p "检测到中国大陆网络，使用国内镜像加速下载？[Y/n] " answer || true
		case "${answer:-Y}" in
		[Nn]*) return 1 ;;
		esac
	fi
	return 0
}

# 每个文件给三个候选：镜像 -> 备用镜像 -> 官方。逐个试，成功即止。
fetch() {
	local file="$1" out="$2"
	local github="https://raw.githubusercontent.com/qeqeq1/proxyService/main/deploy/${file}"
	local jsdelivr="https://cdn.jsdelivr.net/gh/qeqeq1/proxyService@main/deploy/${file}"
	local candidates=()
	if [ "${MIRROR_ACTIVE:-0}" = "1" ]; then
		candidates=("https://gh-proxy.com/${github}" "https://ghfast.top/${github}" "$jsdelivr" "$github")
	else
		candidates=("$github" "https://gh-proxy.com/${github}" "https://ghfast.top/${github}")
	fi
	local url
	for url in "${candidates[@]}"; do
		if command -v curl >/dev/null 2>&1; then
			curl -fsSL --max-time 60 "$url" -o "$out" && return 0
		elif command -v wget >/dev/null 2>&1; then
			wget -q -O "$out" "$url" && return 0
		else
			echo "需要 curl 或 wget 才能下载部署脚本" >&2
			return 1
		fi
		rm -f "$out"
	done
	echo "下载 ${file} 失败，已试过：${candidates[*]}" >&2
	return 1
}

# 只判断一次，避免每个文件都问一遍。
MIRROR_ACTIVE=0
if use_mirror; then
	MIRROR_ACTIVE=1
	export PROXY_MIRROR=cn
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fetch deploy.sh "${work}/deploy.sh"
fetch install.sh "${work}/install.sh"
chmod +x "${work}/deploy.sh" "${work}/install.sh"

"${work}/deploy.sh" "$@"
