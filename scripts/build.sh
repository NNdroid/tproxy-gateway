#!/bin/bash

# 开启报错即退出模式
set -e

# 定义项目名称和输出目录
APP_NAME="tproxy-gateway"
OUT_DIR="bin"

# 定义需要编译的 Linux 架构列表
# 对于 amd64，我们引入了三段式格式: "GOOS/GOARCH/GOAMD64"
TARGETS=(
    "linux/amd64/v1"    # 兼容性最好：传统的 64位 PC/老旧服务器 (等同于标准 amd64)
    "linux/amd64/v2"    # 性能均衡：支持 SSE4.2, POPCNT (如 Intel Nehalem 及之后处理器)
    "linux/amd64/v3"    # 现代主力：支持 AVX, AVX2, BMI2 (绝大多数现代 J4125/N100 软路由、主流服务器服务器推荐)
    "linux/amd64/v4"    # 极客性能：支持 AVX512 (高端 Xeon/EPYC 服务器)
    "linux/arm64"       # 新型 ARM 服务器、树莓派 4/5 64位、Apple Silicon Linux 虚拟机
    "linux/arm"         # 较老的 32位 ARM 设备、树莓派 2/3
    "linux/386"         # 老旧的 32位 x86 设备
    "linux/mipsle"      # 常见的 OpenWrt 路由器 (小端序)
    "linux/mips64le"    # 高性能 OpenWrt 路由器
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
    # 巧妙拆分三段式架构，如果不存在第三段，GOAMD64 会自动为空
    IFS='/' read -r GOOS GOARCH GOAMD64 <<< "$TARGET"
    
    # 根据是否包含 GOAMD64 优化级别，动态决定输出文件名
    if [ -n "$GOAMD64" ]; then
        OUTPUT_NAME="${OUT_DIR}/${APP_NAME}-${GOOS}-${GOARCH}-${GOAMD64}"
        echo "正在编译 -> ${GOOS}/${GOARCH} (${GOAMD64} 优化版) ..."
    else
        OUTPUT_NAME="${OUT_DIR}/${APP_NAME}-${GOOS}-${GOARCH}"
        echo "正在编译 -> ${GOOS}/${GOARCH} ..."
    fi

    # 核心编译命令
    # 动态将 GOAMD64=${GOAMD64} 注入到编译环境变量中
    # CGO_ENABLED=0 : 禁用 CGO，确保生成的是 100% 纯静态链接的二进制文件
    # -ldflags="-s -w" : 剔除符号表和调试信息，大幅减小体积
    # -trimpath : 移除编译机上的绝对路径信息
    CGO_ENABLED=0 GOOS=${GOOS} GOARCH=${GOARCH} GOAMD64=${GOAMD64} \
    go build -trimpath -ldflags="-s -w" -o ${OUTPUT_NAME} .

    echo "   [完成] 生成文件: ${OUTPUT_NAME}"
done

echo "========================================="
echo "🎉 所有平台及优化版本编译均已成功完成！"
echo "编译产物已存放在 ./${OUT_DIR} 目录下："
ls -lh ${OUT_DIR}