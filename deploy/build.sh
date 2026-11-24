#!/usr/bin/env bash
# 交叉编译部署产物。输出到 deploy/out/，可直接拷到服务器。
#
#   ./deploy/build.sh                # 默认三个平台
#   ./deploy/build.sh linux/amd64    # 只编一个
set -euo pipefail

cd "$(dirname "$0")/.."
OUT="deploy/out"
VERSION="$(git describe --tags --always --dirty 2>/dev/null || date +%Y%m%d)"
mkdir -p "$OUT"

build() {
	local goos="$1" goarch="$2"
	local name="njust-proxy-${goos}-${goarch}"
	[ "$goos" = "windows" ] && name="${name}.exe"
	echo "==> ${goos}/${goarch}"
	GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
		go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
		-o "${OUT}/${name}" .
}

if [ "$#" -gt 0 ]; then
	for target in "$@"; do
		build "${target%%/*}" "${target##*/}"
	done
else
	build linux amd64
	build linux arm64
	build windows amd64
fi

echo
echo "产物在 ${OUT}/（版本 ${VERSION}）"
ls -lh "$OUT"
