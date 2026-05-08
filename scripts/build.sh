#!/bin/bash

# 开启报错即退出模式
set -e

# 定义项目名称和输出目录
APP_NAME="tproxy-gateway"
OUT_DIR="bin"

# 定义需要编译的 Linux 架构列表
# 格式: "GOOS/GOARCH"
# 如果你的软路由需要特殊架构（如 mips, mips64），也可以加在下面
TARGETS=(
    "linux/amd64"    # 常见的 64位 PC/服务器 (x86_64)
    "linux/arm64"    # 新型 ARM 服务器、树莓派 4/5 64位、Apple Silicon Linux 虚拟机
    "linux/arm"      # 较老的 32位 ARM 设备、树莓派 2/3
    "linux/386"      # 老旧的 32位 x86 设备
    "linux/mipsle"   # 常见的 OpenWrt 路由器 (小端序)
    "linux/mips64le" # 高性能 OpenWrt 路由器
)

# 清理并创建 bin 目录
echo "清理旧的编译输出..."
rm -rf ${OUT_DIR}
mkdir -p ${OUT_DIR}

echo "========================================="
echo "开始编译 ${APP_NAME} ..."
echo "========================================="

# 遍历目标架构并进行编译
for TARGET in "${TARGETS[@]}"; do
    # 分割 GOOS 和 GOARCH
    GOOS=${TARGET%/*}
    GOARCH=${TARGET#*/}
    
    # 定义输出文件名称，例如: bin/tproxy-gateway-linux-amd64
    OUTPUT_NAME="${OUT_DIR}/${APP_NAME}-${GOOS}-${GOARCH}"

    # 对于 32 位 arm，可以进一步指定 ARM 版本 (可选)
    # export GOARM=7

    echo "正在编译 -> ${GOOS}/${GOARCH} ..."

    # 核心编译命令
    # CGO_ENABLED=0 : 禁用 CGO，确保生成的是 100% 纯静态链接的二进制文件，放到任何 Linux 都不缺依赖库
    # -ldflags="-s -w" : 剔除符号表和调试信息，大幅减小生成的二进制文件体积
    # -trimpath : 移除编译机上的绝对路径信息，保证构建的重现性
    env CGO_ENABLED=0 GOOS=${GOOS} GOARCH=${GOARCH} \
    go build -trimpath -ldflags="-s -w" -o ${OUTPUT_NAME} .

    echo "   [完成] 生成文件: ${OUTPUT_NAME}"
done

echo "========================================="
echo "🎉 所有平台编译均已成功完成！"
echo "编译产物已存放在 ./${OUT_DIR} 目录下："
ls -lh ${OUT_DIR}
