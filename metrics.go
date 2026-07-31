package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

type GlobalMetrics struct {
	DNSQueriesTotal      atomic.Int64
	ActiveTCPConnections atomic.Int64
	ActiveUDPSessions    atomic.Int64
	FakeIPRecordsTotal   atomic.Int64
}

var Metrics GlobalMetrics

func validateAuthToken(r *http.Request) bool {
	if cfg == nil || cfg.UI.Secret == "" {
		return true
	}
	secret := cfg.UI.Secret

	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == secret {
			return true
		}
	}

	if r.Header.Get("X-UI-Token") == secret {
		return true
	}

	if r.URL.Query().Get("token") == secret {
		return true
	}

	return false
}

var startTime = time.Now()

func registerStateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !validateAuthToken(r) {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"status": "unauthorized", "message": "Authentication required"})
			return
		}

		var fakeCIDRs []string
		var uiDomain string
		var uiMode string
		var logLevel string
		if cfg != nil {
			fakeCIDRs = cfg.FakeIP.CIDRs
			uiDomain = cfg.UI.Domain
			uiMode = cfg.UI.Mode
			logLevel = cfg.Log.Level
		}

		uptimeStr := time.Since(startTime).Truncate(time.Second).String()

		dnsCacheCount := 0
		if defaultResolver != nil {
			dnsCacheCount = defaultResolver.CacheCount()
		}

		state := map[string]interface{}{
			"status":                  "running",
			"uptime":                  uptimeStr,
			"dns_queries_total":       Metrics.DNSQueriesTotal.Load(),
			"dns_cache_records_total": dnsCacheCount,
			"active_tcp_conns":        Metrics.ActiveTCPConnections.Load(),
			"active_udp_sessions":     Metrics.ActiveUDPSessions.Load(),
			"fakeip_records_total":    Metrics.FakeIPRecordsTotal.Load(),
			"auth_required":           cfg != nil && cfg.UI.Secret != "",
			"fakeip_cidrs":            fakeCIDRs,
			"ui_domain":               uiDomain,
			"ui_mode":                 uiMode,
			"log_level":               logLevel,
		}
		json.NewEncoder(w).Encode(state)
	})
}

func registerMetricsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if !validateAuthToken(r) {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"status": "unauthorized", "message": "Authentication required"})
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprintf(w, "# HELP tproxy_dns_queries_total Total DNS queries processed by FakeIP server\n")
		fmt.Fprintf(w, "# TYPE tproxy_dns_queries_total counter\n")
		fmt.Fprintf(w, "tproxy_dns_queries_total %d\n\n", Metrics.DNSQueriesTotal.Load())

		dnsCacheCount := 0
		if defaultResolver != nil {
			dnsCacheCount = defaultResolver.CacheCount()
		}
		fmt.Fprintf(w, "# HELP tproxy_dns_cache_records_total Number of cached upstream DNS resolution entries\n")
		fmt.Fprintf(w, "# TYPE tproxy_dns_cache_records_total gauge\n")
		fmt.Fprintf(w, "tproxy_dns_cache_records_total %d\n\n", dnsCacheCount)

		fmt.Fprintf(w, "# HELP tproxy_active_tcp_connections Number of currently active TCP connections\n")
		fmt.Fprintf(w, "# TYPE tproxy_active_tcp_connections gauge\n")
		fmt.Fprintf(w, "tproxy_active_tcp_connections %d\n\n", Metrics.ActiveTCPConnections.Load())

		fmt.Fprintf(w, "# HELP tproxy_active_udp_sessions Number of currently active UDP sessions\n")
		fmt.Fprintf(w, "# TYPE tproxy_active_udp_sessions gauge\n")
		fmt.Fprintf(w, "tproxy_active_udp_sessions %d\n\n", Metrics.ActiveUDPSessions.Load())

		fmt.Fprintf(w, "# HELP tproxy_fakeip_records_total Number of active FakeIP domain mapping records\n")
		fmt.Fprintf(w, "# TYPE tproxy_fakeip_records_total gauge\n")
		fmt.Fprintf(w, "tproxy_fakeip_records_total %d\n", Metrics.FakeIPRecordsTotal.Load())
	})
}

func startMetricsServer(ctx context.Context, mCfg MetricsConfig, uiCfg UIConfig) {
	if !mCfg.Enabled {
		zap.S().Infof("[Metrics] Prometheus metrics service disabled (metrics.enabled: false)")
		return
	}

	if globalWebUIMux == nil {
		initDashboardRoutes(uiCfg, mCfg.Addr)
	}
	mux := globalWebUIMux

	srv := &http.Server{
		Addr:    mCfg.Addr,
		Handler: mux,
	}

	zap.S().Infof("📊 Prometheus Metrics & REST API started -> http://%s/metrics", mCfg.Addr)

	go func() {
		<-ctx.Done()
		zap.S().Infof("[Metrics] Shutting down Metrics server...")
		srv.Shutdown(context.Background())
	}()

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			zap.S().Errorf("[Metrics] Server error: %v", err)
		}
	}()
}

func registerAuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Secret string `json:"secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Invalid request body"})
			return
		}

		if cfg != nil && cfg.UI.Secret != "" && req.Secret != cfg.UI.Secret {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Invalid password"})
			return
		}

		zap.S().Infof("🔐 WebUI authentication successful!")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "success",
			"token":   cfg.UI.Secret,
			"message": "Authentication successful!",
		})
	})
}

func registerConfigRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !validateAuthToken(r) {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"status": "unauthorized", "message": "Authentication required"})
			return
		}

		if r.Method == "GET" {
			data, err := os.ReadFile(ActiveConfigPath)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": err.Error()})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{
				"status":  "success",
				"path":    ActiveConfigPath,
				"content": string(data),
			})
			return
		}

		if r.Method == "POST" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "failed to read request body"})
				return
			}

			newCfg, err := SaveAndValidateConfig(ActiveConfigPath, body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": err.Error()})
				return
			}

			newResolver, rErr := NewDefaultResolver(newCfg.Routing.DefaultDNS)
			if rErr == nil {
				defaultResolver = newResolver
			}

			NewHealthChecker(newCfg.ProxyGroups)
			NewRuleEngine(newCfg.Rules)
			NewAdBlocker(newCfg.AdBlock)
			NewSubscriptionManager(newCfg.Subscriptions)
			cfg = newCfg

			zap.S().Infof("🎉 Configuration saved and hot-reloaded successfully via WebUI!")
			json.NewEncoder(w).Encode(map[string]string{
				"status":  "success",
				"message": "Configuration validated, saved, and hot-reloaded successfully!",
			})
			return
		}

		w.WriteHeader(http.StatusMethodNotAllowed)
	})
}

func registerControlRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/control", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !validateAuthToken(r) {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"status": "unauthorized", "message": "Authentication required"})
			return
		}

		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "invalid request body"})
			return
		}

		switch req.Action {
		case "clear_dns_cache":
			if defaultResolver != nil {
				defaultResolver.ClearCache()
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "success", "message": "DNS resolution cache cleared successfully!"})

		case "clear_fakeip_cache":
			if pool != nil {
				pool.Clear()
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "success", "message": "FakeIP domain mappings cleared successfully!"})

		case "clear_analytics":
			if GlobalAnalytics != nil {
				GlobalAnalytics.Clear()
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "success", "message": "Telemetry analytics data cleared successfully!"})

		case "reload_rules":
			if cfg != nil {
				NewHealthChecker(cfg.ProxyGroups)
				NewRuleEngine(cfg.Rules)
				NewAdBlocker(cfg.AdBlock)
				NewSubscriptionManager(cfg.Subscriptions)
			}
			zap.S().Infof("🔄 Routing rules and proxy health checkers reloaded via WebUI!")
			json.NewEncoder(w).Encode(map[string]string{"status": "success", "message": "Routing rules and proxy checkers reloaded successfully!"})

		case "restart":
			zap.S().Infof("🚀 Restart command received via WebUI! Restarting service...")
			json.NewEncoder(w).Encode(map[string]string{"status": "success", "message": "Gateway restart signal sent! Reconnecting..."})
			go func() {
				time.Sleep(500 * time.Millisecond)
				os.Exit(0)
			}()

		default:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "unknown control action"})
		}
	})
}

func registerLogRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !validateAuthToken(r) {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"status": "unauthorized", "message": "Authentication required"})
			return
		}

		if r.Method == "GET" {
			entries := GlobalLogBuffer.GetEntries(200)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":        "success",
				"current_level": strings.ToUpper(atomicLogLevel.Level().String()),
				"logs":          entries,
			})
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})

	mux.HandleFunc("/api/logs/level", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !validateAuthToken(r) {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"status": "unauthorized", "message": "Authentication required"})
			return
		}

		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Level string `json:"level"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Level == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "invalid log level"})
			return
		}

		newLevel := SetLogLevel(req.Level)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":        "success",
			"current_level": newLevel,
			"message":       fmt.Sprintf("Dynamic log level switched to %s successfully!", newLevel),
		})
	})
}

func PingAllProxies() map[string]map[string]interface{} {
	results := make(map[string]map[string]interface{})
	if cfg == nil {
		return results
	}

	nodes := make(map[string]bool)
	for _, rule := range cfg.Rules {
		if rule.Proxy != "" && strings.ToUpper(rule.Proxy) != "DIRECT" && strings.ToUpper(rule.Proxy) != "REJECT" {
			nodes[rule.Proxy] = true
		}
	}
	for _, pg := range cfg.ProxyGroups {
		for _, p := range pg.Proxies {
			if p != "" && strings.ToUpper(p) != "DIRECT" && strings.ToUpper(p) != "REJECT" {
				nodes[p] = true
			}
		}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex

	for node := range nodes {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			start := time.Now()
			cleanAddr := addr
			if strings.HasPrefix(addr, "socks5://") {
				cleanAddr = strings.TrimPrefix(addr, "socks5://")
			} else if strings.HasPrefix(addr, "http://") {
				cleanAddr = strings.TrimPrefix(addr, "http://")
			}

			conn, err := net.DialTimeout("tcp", cleanAddr, 3*time.Second)
			duration := time.Since(start)

			mu.Lock()
			if err != nil {
				results[addr] = map[string]interface{}{
					"status":  "timeout",
					"latency": "TIMEOUT",
					"ms":      -1,
				}
			} else {
				conn.Close()
				ms := duration.Milliseconds()
				results[addr] = map[string]interface{}{
					"status":  "online",
					"latency": fmt.Sprintf("%d ms", ms),
					"ms":      ms,
				}
			}
			mu.Unlock()
		}(node)
	}

	wg.Wait()
	return results
}

func registerConnectionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/connections", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !validateAuthToken(r) {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"status": "unauthorized", "message": "Authentication required"})
			return
		}

		if r.Method == "GET" {
			list := GlobalConnTracker.List()
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":      "success",
				"count":       len(list),
				"connections": list,
			})
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})

	mux.HandleFunc("/api/connections/kill", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !validateAuthToken(r) {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"status": "unauthorized", "message": "Authentication required"})
			return
		}

		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "invalid connection id"})
			return
		}

		if GlobalConnTracker.Kill(req.ID) {
			zap.S().Infof("✂️ Connection %s forcibly disconnected via WebUI!", req.ID)
			json.NewEncoder(w).Encode(map[string]string{"status": "success", "message": "Connection disconnected successfully!"})
		} else {
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Connection not found or already closed"})
		}
	})

	mux.HandleFunc("/api/proxy/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !validateAuthToken(r) {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"status": "unauthorized", "message": "Authentication required"})
			return
		}

		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		results := PingAllProxies()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "success",
			"nodes":  results,
		})
	})

	mux.HandleFunc("/api/fakeip/lookup", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !validateAuthToken(r) {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"status": "unauthorized", "message": "Authentication required"})
			return
		}

		domain := r.URL.Query().Get("domain")
		if domain == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "domain query parameter is required"})
			return
		}

		ips := pool.GetFakeIP(domain)
		ipStrs := make([]string, len(ips))
		for i, ip := range ips {
			ipStrs[i] = ip.String()
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "success",
			"domain":   domain,
			"fake_ips": ipStrs,
		})
	})
}
