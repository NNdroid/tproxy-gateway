package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
)

var (
	pool            *FakeIPPool
	cfg             *Config
	defaultResolver *DefaultResolver
)

func main() {
	configPath := flag.String("c", "config.yaml", "Specify YAML configuration file path")
	flag.Parse()

	InitLogger("info")

	var err error
	cfg, err = LoadConfig(*configPath)
	if err != nil {
		zap.S().Fatalf("Failed to load config: %v", err)
	}

	InitLogger(cfg.Log.Level)
	zap.S().Infof("Loading configuration from %s...", *configPath)

	defaultResolver, err = NewDefaultResolver(cfg.Routing.DefaultDNS)
	if err != nil {
		zap.S().Fatalf("Failed to initialize default DNS resolver: %v", err)
	}
	zap.S().Infof("Default DNS resolver loaded: [%s] -> %s", defaultResolver.Scheme, defaultResolver.HostPort)

	ttl, _ := time.ParseDuration(cfg.FakeIP.TTL)

	parsedCIDRs, err := cfg.FakeIP.ParseCIDRs()
	if err != nil {
		zap.S().Fatalf("Failed to parse CIDRs: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool = NewFakeIPPool(ctx, parsedCIDRs, ttl, cfg.FakeIP.PersistFile)
	InitAnalytics(ctx, "analytics.json")

	checker := NewHealthChecker(cfg.ProxyGroups)
	checker.Start(ctx)
	NewRuleEngine(cfg.Rules)

	adblocker := NewAdBlocker(cfg.AdBlock)
	go adblocker.StartAutoRefresh(ctx, cfg.AdBlock)

	subMgr := NewSubscriptionManager(cfg.Subscriptions)
	subMgr.Start(ctx)

	setupAutoRoute()

	sigHupChan := make(chan os.Signal, 1)
	signal.Notify(sigHupChan, syscall.SIGHUP)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sigHupChan:
				zap.S().Infof("[HotReload] Received SIGHUP signal, reloading configuration: %s", *configPath)
				newCfg, err := LoadConfig(*configPath)
				if err != nil {
					zap.S().Errorf("[HotReload] Failed to reload config, keeping existing config: %v", err)
					continue
				}

				newResolver, err := NewDefaultResolver(newCfg.Routing.DefaultDNS)
				if err != nil {
					zap.S().Errorf("[HotReload] Failed to initialize new DNS resolver: %v", err)
				} else {
					defaultResolver = newResolver
				}

				NewHealthChecker(newCfg.ProxyGroups)
				NewRuleEngine(newCfg.Rules)
				NewAdBlocker(newCfg.AdBlock)
				NewSubscriptionManager(newCfg.Subscriptions)

				InitLogger(newCfg.Log.Level)
				cfg = newCfg

				zap.S().Infof("🎉 [HotReload] Configuration reloaded successfully! (log level: %s)", strings.ToUpper(cfg.Log.Level))
			}
		}
	}()

	initDashboardRoutes(cfg.UI, cfg.Metrics.Addr)

	go startDNSServer(ctx, cfg.Server.DNSAddr)
	go startTCPTProxy(ctx, cfg.Server.TProxyAddr)
	go startUDPTProxy(ctx, cfg.Server.TProxyAddr)
	go startUDPSweeper(ctx)
	go startInboundProxies(ctx, cfg.Server)
	go startMetricsServer(ctx, cfg.Metrics, cfg.UI)

	zap.S().Infof("🚀 TProxy Gateway started successfully (Log Level: %s)", strings.ToUpper(cfg.Log.Level))

	<-ctx.Done()

	zap.S().Infof("Received shutdown signal, initiating graceful shutdown...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	cleanupDone := make(chan struct{})

	go func() {
		zap.S().Infof("Saving FakeIP cache and shutting down...")
		pool.Close()
		cleanupAutoRoute()
		close(cleanupDone)
	}()

	select {
	case <-cleanupDone:
		zap.S().Infof("🎉 All background tasks and firewall rules cleaned up gracefully. Exiting.")
	case <-shutdownCtx.Done():
		zap.S().Errorf("⚠️ Warning: Cleanup timeout (5s limit exceeded), forcing termination!")
	}

	zap.S().Sync()
}

func setupAutoRoute() {
	setupAutoRouteNetlink()
}

func cleanupAutoRoute() {
	cleanupAutoRouteNetlink()
}
