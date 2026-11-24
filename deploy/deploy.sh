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
if [ ! -f "$binary" ]; then
	if command -v go >/dev/null 2>&1; then
		echo "==> 未找到现成二进制，先构建 linux/${goarch}"
		"${here}/build.sh" "linux/${goarch}"
	else
		echo "既没有 ${binary}，也没有 Go 工具链。" >&2
		echo "在有 Go 的机器上执行 ./deploy/build.sh，再把 deploy/out/ 一起传过来。" >&2
		exit 1
	fi
fi

exec "${here}/install.sh" --binary "$binary" "$@"
