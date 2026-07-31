#!/bin/bash

# 变量定义
GITHUB_REPO="NNdroid/tproxy-gateway"
BIN_PATH="/usr/local/bin/tproxy-gateway"
CONF_DIR="/usr/local/etc/tproxy-gateway"
CONF_FILE="$CONF_DIR/config.yaml"
SERVICE_FILE="/etc/systemd/system/tproxy-gateway.service"

# 检查 root 权限
if [ "$EUID" -ne 0 ]; then
    echo "请使用 root 权限运行此脚本。"
    exit 1
fi

# 获取系统架构与 x86-64 微架构优化级别
get_arch() {
    local arch=$(uname -m)
    case $arch in
        x86_64)
            local flags=$(cat /proc/cpuinfo | grep -m1 "flags" || true)
            if echo "$flags" | grep -q "avx512f" && echo "$flags" | grep -q "avx512bw" && \
               echo "$flags" | grep -q "avx512cd" && echo "$flags" | grep -q "avx512dq" && \
               echo "$flags" | grep -q "avx512vl"; then
                echo "linux-amd64-v4"
            elif echo "$flags" | grep -q "avx" && echo "$flags" | grep -q "avx2" && \
                 echo "$flags" | grep -q "bmi1" && echo "$flags" | grep -q "bmi2" && \
                 echo "$flags" | grep -q "fma" && echo "$flags" | grep -q "movbe"; then
                echo "linux-amd64-v3"
            elif echo "$flags" | grep -q "popcnt" && echo "$flags" | grep -q "sse4_1" && \
                 echo "$flags" | grep -q "sse4_2" && echo "$flags" | grep -q "ssse3"; then
                echo "linux-amd64-v2"
            else
                echo "linux-amd64-v1"
            fi
            ;;
        aarch64) echo "linux-arm64" ;;
        armv7l) echo "linux-arm" ;;
        i386|i686) echo "linux-386" ;;
        mipsle) echo "linux-mipsle" ;;
        mips) echo "linux-mips" ;;
        *) echo "";;
    esac
}

# 安装/更新
install_app() {
    local arch=$(get_arch)
    if [ -z "$arch" ]; then
        echo "不支持的架构: $(uname -m)"
        exit 1
    fi

    echo "检测到最佳匹配架构: $arch"
    echo "正在从 GitHub 获取最新 Release 资讯..."
    
    local release_json=$(curl -s "https://api.github.com/repos/$GITHUB_REPO/releases/latest")
    local download_url=""

    if [[ "$arch" =~ linux-amd64-v([1-4]) ]]; then
        local target_lvl=${BASH_REMATCH[1]}
        for (( lvl=$target_lvl; lvl>=1; lvl-- )); do
            local search_arch="tproxy-gateway-linux-amd64-v${lvl}"
            download_url=$(echo "$release_json" | grep "browser_download_url" | grep "$search_arch" | cut -d '"' -f 4 | head -n 1)
            if [ -n "$download_url" ]; then
                if [ "$lvl" -ne "$target_lvl" ]; then
                    echo "提示: 系统最高支持 v${target_lvl}，已匹配兼容版本: ${search_arch}"
                fi
                break
            fi
        done
    else
        download_url=$(echo "$release_json" | grep "browser_download_url" | grep "tproxy-gateway-$arch" | cut -d '"' -f 4 | head -n 1)
    fi
    
    if [ -z "$download_url" ]; then
        echo "无法找到对应当前架构的下载链接，请检查仓库 Release。"
        exit 1
    fi

    echo "正在下载: $download_url"
    curl -L -o /tmp/tproxy-gateway "$download_url"
    chmod +x /tmp/tproxy-gateway
    mv /tmp/tproxy-gateway "$BIN_PATH"

    mkdir -p "$CONF_DIR"
    if [ ! -f "$CONF_FILE" ]; then
        echo "创建默认配置文件..."
        cat <<EOF > "$CONF_FILE"
log:
  level: "info"
metrics:
  enabled: true
  addr: ":9090"
ui:
  enabled: true
  mode: "both"
  addr: ":9090"
  domain: "dashboard.gateway"
  secret: "admin123"
server:
  dns_addr: ":5353"
  tproxy_addr: "[::]:10800"
  socks_addr: ":1080"
  http_addr: ":8080"
routing:
  default_upstream: "REJECT"
  default_dns: "doh://223.5.5.5/dns-query?sni=dns.alidns.com"
  auto_route: true
  fwmark: 88
  table: 88
  nft_table: "my_custom_overlay_table"
fake_ip:
  cidrs:
    - "198.18.0.0/15"
    - "fd00:6464::/64"
  ttl: "2h"
  persist_file: "$CONF_DIR/fakeip.json"
adblock:
  enabled: true
  urls:
    - "https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts"
rules:
  # I2P 暗网域名通用分流与 User-Agent 重写规则
  - type: "DOMAIN-SUFFIX"
    payload: "i2p"
    proxy: "http://127.0.0.1:4444"
    header_rewrite:
      "User-Agent": "MYOB/6.66 (AN/ON)"
      "Connection": "close"

  # Tor (.onion) 域名分流规则
  - type: "DOMAIN-SUFFIX"
    payload: "onion"
    proxy: "127.0.0.1:9050"

  # 内网私有 IP 强制直连
  - type: "GEOIP"
    payload: "PRIVATE"
    proxy: "DIRECT"

  # 兜底拒绝策略
  - type: "MATCH"
    proxy: "REJECT"
EOF
    fi

    echo "配置 Systemd Service..."
    cat <<EOF > "$SERVICE_FILE"
[Unit]
Description=TProxy Gateway Service
After=network-online.target nss-lookup.target
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
ExecStart=$BIN_PATH -c $CONF_FILE
ExecReload=/bin/kill -HUP \$MAINPID
Restart=on-failure
RestartSec=5s
LimitNOFILE=65535
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    echo "安装/更新完成。"
    echo "使用 'systemctl start tproxy-gateway' 启动服务。"
    echo "控制台 Dashboard 地址: http://IP:9090/ui/"
}

# 卸载
uninstall_app() {
    echo "正在卸载..."
    systemctl stop tproxy-gateway || true
    systemctl disable tproxy-gateway || true
    rm -f "$BIN_PATH"
    rm -f "$SERVICE_FILE"
    systemctl daemon-reload
    echo "卸载完成。"
}

case "$1" in
    install|update)
        install_app
        ;;
    uninstall)
        uninstall_app
        ;;
    *)
        echo "用法: $0 {install|update|uninstall}"
        exit 1
        ;;
esac