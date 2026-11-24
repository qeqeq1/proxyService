#!/usr/bin/env bash
# 真正的一键入口：不 clone、不装 Go，下载脚本后直接部署。
#
#   curl -fsSL https://raw.githubusercontent.com/qeqeq1/proxyService/main/deploy/bootstrap.sh | sudo bash -s -- --origins "https://a.example.com"
#
# 参数原样传给 deploy.sh（--port / --origins / --show-secrets / --uninstall）。
set -euo pipefail

base="${PROXY_BASE_URL:-https://raw.githubusercontent.com/qeqeq1/proxyService/main/deploy}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fetch() {
	local url="$1" out="$2"
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --max-time 60 "$url" -o "$out"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$out" "$url"
	else
		echo "需要 curl 或 wget 才能下载部署脚本" >&2
		exit 1
	fi
}

for file in deploy.sh install.sh; do
	fetch "${base}/${file}" "${work}/${file}"
	chmod +x "${work}/${file}"
done

"${work}/deploy.sh" "$@"
