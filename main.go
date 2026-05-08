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
	router          *DomainRouter
	cfg             *Config
	defaultResolver *DefaultResolver
)

func main() {
	configPath := flag.String("c", "config.yaml", "指定 YAML 配置文件的路徑")
	flag.Parse()

	InitLogger("info")

	var err error
	cfg, err = LoadConfig(*configPath)
	if err != nil {
		zap.S().Fatalf("配置加載中止: %v", err)
	}

	InitLogger(cfg.Log.Level)
	zap.S().Infof("正在從 %s 加載配置...", *configPath)

	defaultResolver, err = NewDefaultResolver(cfg.Routing.DefaultDNS)
	if err != nil {
		zap.S().Fatalf("初始化默認 DNS 失敗: %v", err)
	}
	zap.S().Infof("默認 DNS 已加載: [%s] -> %s", defaultResolver.Scheme, defaultResolver.HostPort)

	ttl, _ := time.ParseDuration(cfg.FakeIP.TTL)
	startIP, ipnet, err := cfg.FakeIP.ParseCIDR()
	if err != nil {
		zap.S().Fatalf("CIDR 錯誤: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool = NewFakeIPPool(ctx, startIP, ipnet, ttl, cfg.FakeIP.PersistFile)
	router = NewDomainRouter()

	for _, rCfg := range cfg.Rules {
		router.AddRule("", rCfg.Proxy, rCfg.HeaderRewrite)
		for _, domain := range rCfg.Domains {
			router.AddRule(domain, rCfg.Proxy, rCfg.HeaderRewrite)
		}
	}

	go startDNSServer(ctx, cfg.Server.DNSAddr)
	go startTCPTProxy(ctx, cfg.Server.TProxyAddr)
	go startUDPTProxy(ctx, cfg.Server.TProxyAddr)
	go startUDPSweeper(ctx)

	zap.S().Infof("🚀 TProxy 網關啟動完成 (日誌級別: %s)", strings.ToUpper(cfg.Log.Level))

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	zap.S().Infof("接收到退出信號，觸發全局 Cancel...")
	cancel()
	time.Sleep(1 * time.Second)

	zap.S().Infof("正在保存緩存並安全關閉...")
	pool.Close()
	zap.S().Sync()
}
