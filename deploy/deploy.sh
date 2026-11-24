#!/usr/bin/env bash
# Linux 一键部署入口：需要时先构建，再交给 install.sh。
#
#   sudo ./deploy/deploy.sh
#   sudo ./deploy/deploy.sh --origins "https://course.example.com"
#
# 参数原样传给 install.sh（--port / --targets / --origins / --admin-user / --binary）。
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"

case "$(uname -m)" in
x86_64 | amd64) goarch="amd64" ;;
aarch64 | arm64) goarch="arm64" ;;
*)
	echo "不支持的架构：$(uname -m)" >&2
	exit 1
	;;
esac

binary="${here}/out/njust-proxy-linux-${goarch}"
release_url="https://github.com/qeqeq1/proxyService/releases/latest/download/njust-proxy-linux-${goarch}"

if [ ! -f "$binary" ]; then
	# 优先用 CI 发布的二进制：部署机不需要 Go 工具链。
	echo "==> 下载 ${release_url}"
	mkdir -p "${here}/out"
	if command -v curl >/dev/null 2>&1; then
		curl -fL --retry 3 --max-time 120 -o "$binary" "$release_url" || rm -f "$binary"
	elif command -v wget >/dev/null 2>&1; then
		wget -O "$binary" "$release_url" || rm -f "$binary"
	fi
	[ -f "$binary" ] && chmod +x "$binary"
fi

if [ ! -f "$binary" ]; then
	if command -v go >/dev/null 2>&1; then
		echo "==> 下载失败，改为本地构建 linux/${goarch}"
		"${here}/build.sh" "linux/${goarch}"
	else
		echo "下载失败，且本机没有 Go 工具链。" >&2
		echo "确认仓库已有 release，或在本机执行 ./deploy/build.sh 后把 deploy/out/ 传过来。" >&2
		exit 1
	fi
fi

exec "${here}/install.sh" --binary "$binary" "$@"
