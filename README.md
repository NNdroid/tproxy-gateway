# 🚀 TProxy Gateway

A lightweight, high-performance, enterprise-grade dual-stack transparent proxy gateway written in Go. Specifically designed for complex network routing, anti-DNS pollution, multi-VPN Overlay networks, and special private network environments (such as I2P and Onion).

---

## ✨ Key Features

* **🛡️ Anti-DNS Pollution (Dual-Stack FakeIP)**:
  Supports both IPv4 (`198.18.0.0/15`) and IPv6 (`fd00::/8`) FakeIP CIDR ranges. Powered by Go `netip.Addr` for **100% zero heap-allocation** lookup, backed by asynchronous atomic disk persistence (`fakeip.json`).
* **⚡ High-Performance DNS Engine (Single-Flight & SWR & DoQ)**:
  * **Single-Flight Deduplication**: Prevents DNS query thundering herd and DNS amplification attacks under high concurrency.
  * **Stale-While-Revalidate (SWR)**: Delivers **0ms instant responses** for stale cache hits while asynchronously revalidating in the background.
  * **Encrypted Upstream Support**: Supports DoQ (DNS over QUIC), DoH (HTTPS), DoT (TLS), TCP, and UDP.
* **🎯 Multi-Type Routing Rule Engine**:
  Supports `DOMAIN-SUFFIX` (Trie tree fast matching), `DOMAIN-KEYWORD`, `DOMAIN-FULL`, `IP-CIDR`, `GEOIP` (`PRIVATE`/`LAN`), and `MATCH` fallback rules.
* **🩺 Proxy Groups & Health Check**:
  * `url-test`: Automatically tests latency and selects the fastest proxy node.
  * `fallback`: Seamless zero-downtime failover to backup nodes.
* **🛡️ AdBlock & Malicious Domain Filtering**:
  Built-in Hosts and AdBlock blocklist parser. Returns `NXDOMAIN` for blocked domains, offering zero-latency ad-blocking across all network devices.
* **🔄 Rule Subscriptions & Zero-Downtime Hot Reload**:
  Supports remote GFWList/Geosite URL rule subscriptions with background auto-refresh and `SIGHUP` signal hot-reloading without dropping TCP/UDP connections.
* **🎨 Embedded WebUI Dashboard & Prometheus Metrics**:
  * **Embedded WebUI**: Native single-page dashboard bundled via Go `//go:embed` (zero external dependencies). Access at `http://IP:9090/ui/`.
  * **Prometheus Metrics**: Exposes `/metrics` endpoint and `/api/state` REST JSON API.
* **🔌 SOCKS5 & HTTP Inbound Proxies**:
  Provides optional SOCKS5 (`:1080`) and HTTP (`:8080`) proxy listeners for non-TProxy devices on the local network.
* **🤖 MCP Server on the WebUI Port**:
  Streamable HTTP at `/mcp`, with 16 tools, 7 resources and 2 diagnostic prompts. Configure independent Bearer, API Key, Basic, inherited WebUI credentials or anonymous access in the dashboard; management tools require `mcp.allow_writes`. See [MCP setup and capabilities](docs/mcp-server.md).
* **📦 Automated Policy Routing (AutoRoute)**:
  Automatically constructs and manages Linux IPv4/IPv6 `ip rule/route` and `nftables` policy chains, leaving zero residue on exit.

---

## 📦 Build and Release Versions

Run `bash scripts/build.sh` to build all supported Linux architectures into `bin/`.
The script works from any directory and generates `v1.0.yyyyMMdd.commitCount-7charHash`,
for example `v1.0.20261003.12-123abcd`:

* `yyyyMMdd`: the HEAD commit's **committer date in UTC**, so rebuilding a commit keeps its version.
* `commitCount`: `git rev-list --count HEAD`, including reachable merge history.
* `7charHash`: exactly the first seven hexadecimal characters of the HEAD commit hash.

Full Git history is required; shallow checkouts must run `git fetch --unshallow` first.
Passing a version to `build.sh` verifies that it matches the generated value; arbitrary overrides are rejected.
Uncommitted edits are not represented in the version: build releases from a clean checkout.

Check a binary with `./bin/tproxy-gateway-linux-amd64-v1 --version`.
Startup logs, the `/api/mcp` discovery response and MCP `initialize.serverInfo.version` use the same injected version.
A plain `go build` without linker flags reports `dev`.

The Release workflow uses these same scripts. Manually run it on the desired branch/ref with `tag_name` left blank
to generate the tag automatically, or push a tag that exactly matches `bash scripts/version.sh`.
An optional manual `tag_name` is checked against the computed version; old date-only or mismatched tags fail validation.
New Release tags point to the exact commit used to build the assets. Ordinary branch pushes run validation CI only.

## ⚙️ Configuration (`config.yaml`)

The gateway is configured via a single YAML file. Refer to the default configuration file for complete details:

👉 **[`config.yaml`](file:///e:/GolandProjects/tproxy-gateway/config.yaml)**

---

## 🖧 Manual nftables & Policy Routing

*(Note: When `routing.auto_route: true` is enabled, these rules are applied and cleaned up automatically on start/exit)*

```nftables
# /etc/nftables-tproxy.conf example
table inet tproxy_gw {
    chain prerouting {
        type filter hook prerouting priority mangle; policy accept;
        
        # Intercept IPv4 FakeIP (198.18.0.0/15)
        ip daddr 198.18.0.0/15 meta l4proto { tcp, udp } tproxy ip to 127.0.0.1:10800 meta mark set 88 accept
        
        # Intercept IPv6 FakeIP (fd00::/8)
        ip6 daddr fd00::/8 meta l4proto { tcp, udp } tproxy ip6 to [::1]:10800 meta mark set 88 accept
    }
}
```

Policy Routing:
```bash
# IPv4 Policy Route
ip rule add fwmark 88/88 table 88
ip route add local 0.0.0.0/0 dev lo table 88

# IPv6 Policy Route
ip -6 rule add fwmark 88/88 table 88
ip -6 route add local ::/0 dev lo table 88
```

---

## 🔄 Zero-Downtime Hot Reload

To reload configuration without restarting the process or dropping active connections:

```bash
kill -HUP $(pgrep tproxy-gateway)
```

---

## ⚠️ Disclaimer

This project is intended strictly for network communication technology learning and research. Please comply with the laws and regulations of your country or region. Do not use this software for any illegal purposes.

---
---
