#!/bin/bash

# 配置變量
GITHUB_REPO="NNdroid/tproxy-gateway"
BIN_PATH="/usr/local/bin/tproxy-gateway"
CONF_DIR="/usr/local/etc/tproxy-gateway"
CONF_FILE="$CONF_DIR/config.yaml"
SERVICE_FILE="/etc/systemd/system/tproxy-gateway.service"

# 檢查權限
if [ "$EUID" -ne 0 ]; then
    echo "請使用 root 權限運行此腳本。"
    exit 1
fi

# 獲取系統架構與 x86-64 微架構優化級別
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
        *) echo "";;
    esac
}

# 安裝/更新
install_app() {
    local arch=$(get_arch)
    if [ -z "$arch" ]; then
        echo "不支援的架構: $(uname -m)"
        exit 1
    fi

    echo "檢測到最佳匹配架構: $arch"
    echo "正在從 GitHub 獲取最新 Release 資訊..."
    
    local release_json=$(curl -s "https://api.github.com/repos/$GITHUB_REPO/releases/latest")
    local download_url=""

    if [[ "$arch" =~ linux-amd64-v([1-4]) ]]; then
        local target_lvl=${BASH_REMATCH[1]}
        for (( lvl=$target_lvl; lvl>=1; lvl-- )); do
            local search_arch="linux-amd64-v${lvl}"
            download_url=$(echo "$release_json" | grep "browser_download_url" | grep "$search_arch" | cut -d '"' -f 4 | head -n 1)
            if [ -n "$download_url" ]; then
                if [ "$lvl" -ne "$target_lvl" ]; then
                    echo "提示: 系統最高支援 v${target_lvl}，但在 Release 中未找到該編譯版，已安全匹配到相容的: ${search_arch}"
                fi
                break
            fi
        done
    else
        download_url=$(echo "$release_json" | grep "browser_download_url" | grep "$arch" | cut -d '"' -f 4 | head -n 1)
    fi
    
    if [ -z "$download_url" ]; then
        echo "無法找到對應當前架構或相容架構的下載連結，請檢查倉庫 Release 命名。"
        exit 1
    fi

    echo "正在下載: $download_url"
    curl -L -o /tmp/tproxy-gateway "$download_url"
    chmod +x /tmp/tproxy-gateway
    mv /tmp/tproxy-gateway "$BIN_PATH"

    # 建立配置目錄
    mkdir -p "$CONF_DIR"
    if [ ! -f "$CONF_FILE" ]; then
        echo "建立默認配置文件..."
        cat <<EOF > "$CONF_FILE"
log:
  level: "warn"
server:
  dns_addr: ":5353"
  tproxy_addr: "[::]:10800"
routing:
  default_upstream: "REJECT"
  default_dns: "doh://8.8.8.8/dns-query?sni=dns.google"
  auto_route: true
  fwmark: 88
  table: 88
  nft_table: "my_custom_overlay_table"
fake_ip:
  cidrs:
    - "fd99:e21::/64"
  ttl: "36h"
  persist_file: "$CONF_DIR/fakeip.json"
rules:
  - proxy: "127.0.0.1:4447"
    domains:
      - "i2p"
    header_rewrite:
      "User-Agent": "MYOB/6.66 (AN/ON)"
  - proxy: "127.0.0.1:9150"
    domains:
      - "onion"
EOF
    fi

    # 寫入並整合 Service 文件
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

# 变动点：网络规则控制完全收回到 Go 内部，此处无须任何硬编码的前置脚本执行项
ExecStart=$BIN_PATH -c $CONF_FILE

Restart=on-failure
RestartSec=5s
LimitNOFILE=65535
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    echo "安裝/更新完成。"
    echo "使用 'systemctl start tproxy-gateway' 啟動服務。"
}

# 卸載
uninstall_app() {
    echo "正在卸載..."
    systemctl stop tproxy-gateway || true
    systemctl disable tproxy-gateway || true
    rm -f "$BIN_PATH"
    rm -f "$SERVICE_FILE"
    # 如果要保留配置，請註釋掉下面一行
    # rm -rf "$CONF_DIR"
    systemctl daemon-reload
    echo "卸載完成。"
}

# 菜單
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