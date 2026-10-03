package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"tproxy-gateway/internal/mcpauth"
)

func validateMCPConfig(ctx context.Context, content string) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	decoder := yaml.NewDecoder(strings.NewReader(content))
	decoder.KnownFields(true)
	var candidate Config
	if err := decoder.Decode(&candidate); err != nil {
		return nil, err
	}
	if err := mcpauth.Validate(candidate.MCP.Auth); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("configuration must contain exactly one YAML document")
	}
	parsed, err := candidate.FakeIP.ParseCIDRs()
	if err != nil {
		return nil, err
	}
	if candidate.FakeIP.TTL != "" {
		ttl, err := time.ParseDuration(candidate.FakeIP.TTL)
		if err != nil || ttl <= 0 {
			return nil, errors.New("FakeIP TTL must be a positive duration")
		}
	}
	if candidate.UI.Mode != "" && candidate.UI.Mode != "port" && candidate.UI.Mode != "domain" && candidate.UI.Mode != "both" {
		return nil, errors.New("ui.mode must be port, domain or both")
	}
	if candidate.Routing.DefaultDNS != "" {
		raw := candidate.Routing.DefaultDNS
		if !strings.Contains(raw, "://") {
			raw = "udp://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			return nil, errors.New("invalid routing.default_dns URL")
		}
		switch u.Scheme {
		case "udp", "tcp", "dot", "doh", "doq":
		default:
			return nil, errors.New("unsupported routing.default_dns scheme")
		}
	}
	protection := http.NewCrossOriginProtection()
	for _, origin := range candidate.MCP.AllowedOrigins {
		if err := protection.AddTrustedOrigin(origin); err != nil {
			return nil, err
		}
	}
	restart := make([]string, 0)
	if cfg != nil {
		if !reflect.DeepEqual(candidate.Server, cfg.Server) {
			restart = append(restart, "server listeners")
		}
		if !reflect.DeepEqual(candidate.FakeIP, cfg.FakeIP) {
			restart = append(restart, "FakeIP pool")
		}
		if candidate.UI.Enabled != cfg.UI.Enabled || candidate.UI.Mode != cfg.UI.Mode || candidate.UI.Addr != cfg.UI.Addr || candidate.UI.Domain != cfg.UI.Domain || candidate.Metrics != cfg.Metrics {
			restart = append(restart, "management listeners")
		}
		if candidate.Routing.Fwmark != cfg.Routing.Fwmark || candidate.Routing.Table != cfg.Routing.Table || candidate.Routing.NftTable != cfg.Routing.NftTable || candidate.Routing.AutoRoute != cfg.Routing.AutoRoute {
			restart = append(restart, "kernel routing")
		}
		if !slices.Equal(candidate.MCP.AllowedOrigins, cfg.MCP.AllowedOrigins) {
			restart = append(restart, "MCP allowed origins")
		}
	}
	return map[string]any{"valid": true, "restart_required": restart, "rules": len(candidate.Rules), "proxy_groups": len(candidate.ProxyGroups), "ipv4_prefixes": len(parsed.V4Prefixes), "ipv6_prefixes": len(parsed.V6Prefixes)}, nil
}

// Management services always share one physical listener. Domain-only UI keeps
// using the virtual-domain mux unless metrics explicitly requires a listener.
func managementListenAddress(metrics MetricsConfig, ui UIConfig, mcpEnabled bool) (string, bool) {
	mode := strings.ToLower(strings.TrimSpace(ui.Mode))
	uiPort := ui.Enabled && mode != "domain"
	if !metrics.Enabled && !uiPort && (!mcpEnabled || ui.Enabled && mode == "domain") {
		return "", false
	}
	address := metrics.Addr
	if (!metrics.Enabled || address == "") && uiPort && ui.Addr != "" {
		address = ui.Addr
	}
	if address == "" {
		address = ":9090"
	}
	return address, true
}
