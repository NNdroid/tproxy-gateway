package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"

	"go.uber.org/zap"
	"tproxy-gateway/internal/gatewaymcp"
	"tproxy-gateway/internal/mcpauth"
)

func registerMCPRoutes(mux *http.ServeMux) {
	publishMCPPolicy(cfg)
	options := gatewaymcp.Options{
		Backend: mux,
		Policy: func() gatewaymcp.Policy {
			policy := currentMCPPolicy()
			return gatewaymcp.Policy{Auth: policy.Config.Auth, UISecret: policy.UISecret, Enabled: policy.Config.Enabled, AllowWrites: policy.Config.AllowWrites}
		},
		ValidateConfig: validateMCPConfig,
		CheckDomain: func(ctx context.Context, domain, target string) (map[string]any, error) {
			if cfg == nil || globalRuleEngine == nil {
				return nil, errors.New("routing engine is not initialized")
			}
			ip, _ := netip.ParseAddr(target)
			upstream, _ := globalRuleEngine.Match(domain, ip.Unmap())
			selected := upstream
			if globalChecker != nil {
				selected = globalChecker.SelectProxy(upstream)
			}
			return map[string]any{"domain": domain, "target_ip": target, "adblock_blocked": globalAdBlocker.IsBlocked(domain), "rule_upstream": redactProxyAddress(upstream), "selected_upstream": redactProxyAddress(selected)}, nil
		},
		LookupDNS: func(ctx context.Context, domain string) (map[string]any, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if defaultResolver == nil {
				return nil, errors.New("DNS resolver is not initialized")
			}
			ips, err := defaultResolver.LookupIP(domain)
			if err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			addresses := make([]string, 0, len(ips))
			for _, ip := range ips {
				addresses = append(addresses, ip.String())
			}
			return map[string]any{"domain": domain, "ips": addresses}, nil
		},
		ReverseFakeIP: func(ctx context.Context, address string) (map[string]any, error) {
			if pool == nil {
				return nil, errors.New("FakeIP pool is not initialized")
			}
			ip, _ := netip.ParseAddr(address)
			ip = ip.Unmap()
			shard := &pool.ipShards[fnv32Addr(ip)%ShardCount]
			shard.mu.RLock()
			record := shard.m[ip]
			domain := ""
			if record != nil {
				domain = record.Domain
			}
			shard.mu.RUnlock()
			return map[string]any{"ip": ip.String(), "found": record != nil, "domain": domain}, nil
		},
	}
	if cfg != nil {
		options.AllowedOrigins = append([]string(nil), cfg.MCP.AllowedOrigins...)
	}
	handler, err := gatewaymcp.NewHandler(options)
	if err != nil {
		zap.S().Fatalf("Failed to initialize MCP server: %v", err)
	}
	mux.Handle("/mcp", handler)
	mux.HandleFunc("/mcp/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/" {
			http.NotFound(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	})
	mux.HandleFunc("/api/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !validateAuthToken(r) {
			http.Error(w, "Authentication required", http.StatusUnauthorized)
			return
		}
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"enabled": currentMCPPolicy().Config.Enabled, "allow_writes": currentMCPPolicy().Config.AllowWrites,
			"endpoint": "/mcp", "transport": "streamable-http", "stateless": true,
			"tools": gatewaymcp.ToolNames, "resources": 7, "prompts": 2,
		})
	})
	mux.Handle("/api/mcp/settings", mcpauth.NewSettingsHandler(mcpauth.SettingsOptions{
		Authorized: func(r *http.Request) bool {
			return mcpauth.Authenticate(r, mcpauth.Config{}, currentMCPPolicy().UISecret)
		},
		Load: func() mcpauth.Config { return currentMCPPolicy().Config.Auth },
		Save: func(ctx context.Context, update mcpauth.Update) (mcpauth.Config, error) {
			return saveMCPAuthentication(ctx, ActiveConfigPath, update)
		},
	}))
}

func redactProxyAddress(address string) string {
	if index := strings.LastIndexByte(address, '@'); index >= 0 {
		if scheme := strings.Index(address, "://"); scheme >= 0 {
			return address[:scheme+3] + "REDACTED@" + address[index+1:]
		}
		return "REDACTED@" + address[index+1:]
	}
	return address
}
