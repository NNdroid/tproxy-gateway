#!/bin/bash

# 配置变量
GITHUB_REPO="NNdroid/tproxy-gateway"
BIN_PATH="/usr/local/bin/tproxy-gateway"
CONF_DIR="/usr/local/etc/tproxy-gateway"
CONF_FILE="$CONF_DIR/config.yaml"
SERVICE_FILE="/etc/systemd/system/tproxy-gateway.service"
NFT_CONF="/etc/nftables-tproxy.conf"

# 检查权限
if [ "$EUID" -ne 0 ]; then
    echo "请使用 root 权限运行此脚本。"
    exit 1
fi

# 获取系统架构
get_arch() {
    local arch=$(uname -m)
    case $arch in
        x86_64) echo "linux-amd64" ;;
        aarch64) echo "linux-arm64" ;;
        armv7l) echo "linux-arm" ;;
        i386|i686) echo "linux-386" ;;
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

    echo "检测到架构: $arch"
    echo "正在从 GitHub 获取最新 Release..."
    
    # 获取下载 URL
    local download_url=$(curl -s https://api.github.com/repos/$GITHUB_REPO/releases/latest | grep "browser_download_url" | grep "$arch" | cut -d '"' -f 4)
    
    if [ -z "$download_url" ]; then
        echo "无法找到对应架构的下载链接，请检查仓库 Release 命名。"
        exit 1
    fi

    echo "正在下载: $download_url"
    curl -L -o /tmp/tproxy-gateway "$download_url"
    chmod +x /tmp/tproxy-gateway
    mv /tmp/tproxy-gateway "$BIN_PATH"

    # 创建配置目录
    mkdir -p "$CONF_DIR"
    if [ ! -f "$CONF_FILE" ]; then
        echo "创建默认配置文件..."
        cat <<EOF > "$CONF_FILE"
log:
  level: "info"
server:
  dns_addr: ":5353"
  tproxy_addr: "[::]:10800"
routing:
  default_upstream: "REJECT"
  default_dns: "doh://223.5.5.5/dns-query?sni=dns.alidns.com"
fake_ip:
  cidr: "fd00::/8"
  ttl: "2h"
  persist_file: "$CONF_DIR/fakeip.json"
rules:
  - proxy: "127.0.0.1:1080"
    domains:
      - "google.com"
EOF
    fi

    # 配置 nftables (基于你提供的配置)
    echo "配置 nftables 规则..."
    cat <<EOF > "$NFT_CONF"
table inet tproxy_gw {
    chain prerouting {
        type filter hook prerouting priority mangle; policy accept;
        # 这里的 IP 段建议根据 config.yaml 中的 fake_ip.cidr 自动或手动同步
        # 下面演示根据你之前的配置，将流量转发到 [::1]:10800 (tproxy_addr)
        ip6 daddr fd00::/8 meta l4proto { tcp, udp } tproxy ip6 to [::1]:10800 meta mark set 1 accept
    }
}
EOF

    # 写入并整合 Service 文件
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

# 启动前初始化路由表 1 的规则
ExecStartPre=-/usr/sbin/ip -6 rule add fwmark 1/1 table 1
ExecStartPre=-/usr/sbin/ip -6 route add local ::/0 dev lo table 1
# 加载 nftables 规则
ExecStartPre=/usr/sbin/nft -f $NFT_CONF

ExecStart=$BIN_PATH -c $CONF_FILE

# 停止后清理路由规则和 nftables
ExecStopPost=-/usr/sbin/ip -6 rule del fwmark 1/1 table 1
ExecStopPost=-/usr/sbin/ip -6 route del local ::/0 dev lo table 1
ExecStopPost=-/usr/sbin/nft delete table inet tproxy_gw

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
}

# 卸载
uninstall_app() {
    echo "正在卸载..."
    systemctl stop tproxy-gateway
    systemctl disable tproxy-gateway
    rm -f "$BIN_PATH"
    rm -f "$SERVICE_FILE"
    rm -f "$NFT_CONF"
    # 如果要保留配置，请注释掉下面一行
    # rm -rf "$CONF_DIR"
    systemctl daemon-reload
    echo "卸载完成。"
}

# 菜单
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