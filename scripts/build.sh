#!/usr/bin/env bash

# 开启报错即退出模式
set -euo pipefail

# 定义项目名称和输出目录
APP_NAME="tproxy-gateway"
PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PROJECT_ROOT"
OUT_DIR="$PROJECT_ROOT/bin"
VERSION="$(bash "$PROJECT_ROOT/scripts/version.sh" "$@")"
# Do not depend on a user's global GOPATH-mode setting.
export GO111MODULE=on

# 定义需要编译的 Linux 架构列表
TARGETS=(
    "linux/amd64/v1"
    "linux/amd64/v2"
    "linux/amd64/v3"
    "linux/amd64/v4"
    "linux/arm64"
    "linux/arm"
    "linux/386"
    "linux/mipsle"
    "linux/mips"
    "linux/mips64le"
)

echo "清理旧的编译输出..."
rm -rf -- "$OUT_DIR"
mkdir -p -- "$OUT_DIR"

echo "========================================="
echo "开始编译 ${APP_NAME} (${VERSION}) ..."
echo "========================================="

for TARGET in "${TARGETS[@]}"; do
    IFS='/' read -r GOOS GOARCH GOAMD64 <<< "$TARGET"
    
    if [ -n "$GOAMD64" ]; then
        OUTPUT_NAME="${OUT_DIR}/${APP_NAME}-${GOOS}-${GOARCH}-${GOAMD64}"
        echo "正在编译 -> ${GOOS}/${GOARCH} (${GOAMD64}) ..."
    else
        OUTPUT_NAME="${OUT_DIR}/${APP_NAME}-${GOOS}-${GOARCH}"
        echo "正在编译 -> ${GOOS}/${GOARCH} ..."
    fi

    CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" GOAMD64="${GOAMD64:-}" \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o "$OUTPUT_NAME" .

    echo "   [完成] 生成文件: ${OUTPUT_NAME}"
done

echo "========================================="
echo "🎉 所有平台及优化版本编译均已成功完成！"
echo "编译产物已存放在 ${OUT_DIR} 目录下："
ls -lh "$OUT_DIR"
