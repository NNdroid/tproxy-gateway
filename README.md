# 🚀 TProxy Gateway

A lightweight, high-performance, enterprise-grade transparent proxy gateway written in Go. It is specifically designed to handle complex network routing, prevent DNS pollution, and seamlessly navigate special network environments (such as I2P and Onion).

## ✨ Core Features

* **🛡️ Anti-DNS Pollution (FakeIP)**: Seamless FakeIP resource pool based on IPv6 (`fd00::/8`), supporting asynchronous disk persistence so your cache survives reboots.
* **🌐 Full Protocol Support**: Complete takeover and forwarding of both TCP and UDP traffic, powered by the Linux kernel's `TPROXY` module.
* **⚡ High-Performance Routing**: Built-in Trie (prefix tree) domain matching algorithm, maintaining microsecond-level latency even with millions of routing rules.
* **✍️ HTTP Header Rewriting**: Supports L7 application-layer header modification (e.g., dynamic `Host` rewriting) for pure HTTP traffic on port 80.
* **🔒 Secure Direct DNS**: High-performance built-in DNS resolver. The direct connection strategy supports DoH (DNS over HTTPS), DoT (DNS over TLS), and concurrent IPv4/IPv6 (Happy Eyeballs) resolution to maximize speed.
* **📦 One-Click Deployment**: Comes with a fully automated management script that handles binary downloading, `systemd` service configuration, and `nftables` policy routing.

---

## ⚙️ Configuration Guide

The gateway is configured via a single YAML file. 

For a complete list of options—including routing rules, FakeIP settings, upstream proxy authentication, and HTTP header rewriting—please refer to the default configuration file located in the project directory:

**`config.yaml`**

*(Note: If deployed via the `install.sh` script, the active configuration file is installed at `/usr/local/etc/tproxy-gateway/config.yaml`)*

---

## 🖧 Manual nftables & Policy Routing

*Note: If you use the provided `install.sh` script, these rules are applied and managed automatically via systemd. The details below are for manual configuration or advanced custom setups.*

For `TPROXY` to intercept traffic successfully, you must configure `nftables` to mark the packets and set up policy routing to deliver those marked packets locally.

### 1. nftables Configuration
Create a configuration file (e.g., `/etc/nftables-tproxy.conf`) with the following content to hijack traffic destined for the FakeIP CIDR:

```nftables
table inet tproxy_gw {
    chain prerouting {
        type filter hook prerouting priority mangle; policy accept;
        
        # Intercept TCP/UDP traffic destined for the FakeIP CIDR (fd00::/8)
        # Forward it to the local TProxy port (10800) and set firewall mark (fwmark) to 1
        ip6 daddr fd00::/8 meta l4proto { tcp, udp } tproxy ip6 to [::1]:10800 meta mark set 1 accept
    }
}

```

Apply the rules: `nft -f /etc/nftables-tproxy.conf`

### 2. Policy Routing

TPROXY requires a custom routing table to ensure the marked packets are routed to the local loopback interface. Execute the following `iproute2` commands:

```bash
# Create a rule to route packets with fwmark 1 to table 1
ip -6 rule add fwmark 1/1 table 1

# Add a local route to table 1 to deliver the packets locally
ip -6 route add local ::/0 dev lo table 1

```

---

## 🧠 Advanced Usage: Accessing I2P / Onion Networks

Because modern browsers often treat special top-level domains (like `.i2p` and `.onion`) as insecure non-standard origins, they may block pure HTTP requests. If you want to seamlessly access these networks via pure HTTP through this transparent gateway, launch your browser with the following command-line flags:

**Google Chrome / Microsoft Edge:**

```bash
--unsafely-treat-insecure-origin-as-secure="http://*.i2p,http://*.onion"

```

---

## 🏗️ Network Architecture Overview

1. **DNS Hijacking Phase**: The client initiates a DNS query. `tproxy-gateway` intercepts it and assigns an IPv6 FakeIP (e.g., `fd00::13`).
2. **Traffic Routing Phase**: The client initiates a TCP/UDP connection to `fd00::13`. Linux `nftables` marks the packet (`fwmark 1`) and routes the traffic to the local `10800` TProxy port.
3. **Gateway Processing Phase**:
* The gateway performs a reverse lookup on the FakeIP to retrieve the real domain.
* It matches the domain against the routing `rules` using the Trie tree.
* **DIRECT**: Concurrently dispatches A / AAAA queries (with built-in caching) and connects directly to the target.
* **PROXY**: Reuses the SOCKS5 Client pool to encapsulate and forward traffic to the upstream proxy.
* **HTTP REWRITE**: If the target port is 80 and `header_rewrite` is configured, it dynamically modifies the HTTP packets.



---

## ⚠️ Disclaimer

This project is intended strictly for network communication technology learning and research. Please comply with the laws and regulations of your country or region. Do not use this software for any illegal purposes.