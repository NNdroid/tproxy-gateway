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
	configPath := flag.String("c", "config.yaml", "指定 YAML 配置文件的路径")
	flag.Parse()

	// 临时初始化简易 Logger
	InitLogger("info")

	var err error
	cfg, err = LoadConfig(*configPath)
	if err != nil {
		zap.S().Fatalf("配置加载中止: %v", err)
	}

	// 按配置重新初始化全局 Logger
	InitLogger(cfg.Log.Level)
	zap.S().Infof("正在从 %s 加载配置...", *configPath)

	defaultResolver, err = NewDefaultResolver(cfg.Routing.DefaultDNS)
	if err != nil {
		zap.S().Fatalf("初始化默认 DNS 失败: %v", err)
	}
	zap.S().Infof("默认 DNS 已加载: [%s] -> %s", defaultResolver.Scheme, defaultResolver.HostPort)

	ttl, _ := time.ParseDuration(cfg.FakeIP.TTL)
	startIP, ipnet, err := cfg.FakeIP.ParseCIDR()
	if err != nil {
		zap.S().Fatalf("CIDR 错误: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool = NewFakeIPPool(ctx, startIP, ipnet, ttl, cfg.FakeIP.PersistFile)
	router = NewDomainRouter()

	for _, rCfg := range cfg.Rules {
		upstream := rCfg.Proxy
		if strings.ToUpper(upstream) == "DIRECT" {
			upstream = ""
		}
		for _, domain := range rCfg.Domains {
			router.AddRule(domain, upstream)
		}
	}

	go startDNSServer(ctx, cfg.Server.DNSAddr)
	go startTCPTProxy(ctx, cfg.Server.TProxyAddr)
	go startUDPTProxy(ctx, cfg.Server.TProxyAddr)
	go startUDPSweeper(ctx)

	zap.S().Infof("🚀 TProxy 网关启动完成 (日志级别: %s)", strings.ToUpper(cfg.Log.Level))

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	zap.S().Infof("接收到退出信号，触发全局 Cancel...")
	cancel()
	time.Sleep(1 * time.Second)

	zap.S().Infof("正在保存缓存并安全关闭...")
	pool.Close()
	zap.S().Sync()
}