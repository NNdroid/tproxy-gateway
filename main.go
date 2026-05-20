package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os/exec"
	"os/signal"
	"strconv"
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

	startIPs, ipnets, err := cfg.FakeIP.ParseCIDRs()
	if err != nil {
		zap.S().Fatalf("CIDR 錯誤: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool = NewFakeIPPool(ctx, startIPs, ipnets, ttl, cfg.FakeIP.PersistFile)
	router = NewDomainRouter()

	for _, rCfg := range cfg.Rules {
		router.AddRule("", rCfg.Proxy, rCfg.HeaderRewrite)
		for _, domain := range rCfg.Domains {
			router.AddRule(domain, rCfg.Proxy, rCfg.HeaderRewrite)
		}
	}

	setupAutoRoute()

	go startDNSServer(ctx, cfg.Server.DNSAddr)
	go startTCPTProxy(ctx, cfg.Server.TProxyAddr)
	go startUDPTProxy(ctx, cfg.Server.TProxyAddr)
	go startUDPSweeper(ctx)

	zap.S().Infof("🚀 TProxy 網關啟動完成 (日誌級別: %s)", strings.ToUpper(cfg.Log.Level))

	<-ctx.Done()

	zap.S().Infof("接收到退出信號，啟動平滑關閉流程...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	
	cleanupDone := make(chan struct{})

	go func() {
		zap.S().Infof("正在保存快取並安全關閉...")
		pool.Close()         // 資料強制無鎖安全落盘
		cleanupAutoRoute()   // 清理策略路由與防火牆規則
		close(cleanupDone)
	}()

	select {
	case <-cleanupDone:
		zap.S().Infof("🎉 所有後台任務與防火牆環境已完美恢復純淨，安全退出。")
	case <-shutdownCtx.Done():
		zap.S().Errorf("⚠️ 警告: 清理超時（超過 5 秒），為防進程掛起，網關實施強行終止！")
	}

	zap.S().Sync()
}

func setupAutoRoute() {
	if !cfg.Routing.AutoRoute {
		return
	}
	zap.S().Infof("[AutoRoute] 托管启动 -> 路由表:%d | 防火墙Mark:%d | Nftables表名:%s", cfg.Routing.Table, cfg.Routing.Fwmark, cfg.Routing.NftTable)

	tableStr := strconv.Itoa(cfg.Routing.Table)
	markStr := strconv.Itoa(cfg.Routing.Fwmark)

	exec.Command("ip", "-6", "rule", "add", "fwmark", markStr+"/"+markStr, "table", tableStr).Run()
	exec.Command("ip", "-6", "route", "add", "local", "::/0", "dev", "lo", "table", tableStr).Run()

	_, port, err := net.SplitHostPort(cfg.Server.TProxyAddr)
	if err != nil {
		idx := strings.LastIndex(cfg.Server.TProxyAddr, ":")
		if idx != -1 {
			port = cfg.Server.TProxyAddr[idx+1:]
		} else {
			port = "10800"
		}
	}

	var nftRules strings.Builder
	nftRules.WriteString(fmt.Sprintf("table inet %s {\n", cfg.Routing.NftTable))
	nftRules.WriteString("    chain prerouting {\n")
	nftRules.WriteString("        type filter hook prerouting priority mangle; policy accept;\n")
	for _, cidr := range cfg.FakeIP.CIDRs {
		nftRules.WriteString(fmt.Sprintf("        ip6 daddr %s meta l4proto { tcp, udp } tproxy ip6 to [::1]:%s meta mark set %s accept\n", cidr, port, markStr))
	}
	nftRules.WriteString("    }\n")
	nftRules.WriteString("}\n")

	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(nftRules.String())
	if err := cmd.Run(); err != nil {
		zap.S().Errorf("[AutoRoute] 动态加载 nftables 失败: %v", err)
	} else {
		zap.S().Infof("[AutoRoute] nftables 防火墙托管链构建成功。")
	}
}

func cleanupAutoRoute() {
	if !cfg.Routing.AutoRoute {
		return
	}
	zap.S().Infof("[AutoRoute] 正在清理策略路由与 nftables 规则...")

	tableStr := strconv.Itoa(cfg.Routing.Table)
	markStr := strconv.Itoa(cfg.Routing.Fwmark)

	exec.Command("ip", "-6", "rule", "del", "fwmark", markStr+"/"+markStr, "table", tableStr).Run()
	exec.Command("ip", "-6", "route", "del", "local", "::/0", "dev", "lo", "table", tableStr).Run()

	exec.Command("nft", "delete", "table", "inet", cfg.Routing.NftTable).Run()
	zap.S().Infof("[AutoRoute] 防火墙与路由表环境恢复纯净。")
}
