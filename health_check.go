package main

import (
	"context"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

type ProxyNodeStatus struct {
	Addr      string        `json:"addr"`
	Alive     bool          `json:"alive"`
	RTT       time.Duration `json:"rtt"`
	LastCheck time.Time     `json:"last_check"`
	FailCount int           `json:"fail_count"`
}

type ProxyGroup struct {
	Name     string        `json:"name"`
	Type     string        `json:"type"`
	Proxies  []string      `json:"proxies"`
	URL      string        `json:"url"`
	Interval time.Duration `json:"interval"`
	Selected string        `json:"selected"`
	mu       sync.RWMutex
}

type HealthChecker struct {
	nodes  map[string]*ProxyNodeStatus
	groups map[string]*ProxyGroup
	mu     sync.RWMutex
}

var globalChecker *HealthChecker

func NewHealthChecker(groupConfigs []ProxyGroupConfig) *HealthChecker {
	hc := &HealthChecker{
		nodes:  make(map[string]*ProxyNodeStatus),
		groups: make(map[string]*ProxyGroup),
	}

	for _, gCfg := range groupConfigs {
		nameUpper := strings.ToUpper(strings.TrimSpace(gCfg.Name))
		if nameUpper == "DIRECT" || nameUpper == "REJECT" {
			zap.S().Errorf("[HealthCheck] Proxy group name cannot use reserved keyword DIRECT or REJECT: %s", gCfg.Name)
			continue
		}

		interval, err := time.ParseDuration(gCfg.Interval)
		if err != nil || interval <= 0 {
			interval = 30 * time.Second
		}
		testURL := gCfg.URL
		if testURL == "" {
			testURL = "http://cp.cloudflare.com/generate_204"
		}

		group := &ProxyGroup{
			Name:     gCfg.Name,
			Type:     gCfg.Type,
			Proxies:  gCfg.Proxies,
			URL:      testURL,
			Interval: interval,
		}
		if len(gCfg.Proxies) > 0 {
			group.Selected = gCfg.Proxies[0]
		}
		hc.groups[gCfg.Name] = group

		for _, pAddr := range gCfg.Proxies {
			if _, exists := hc.nodes[pAddr]; !exists {
				hc.nodes[pAddr] = &ProxyNodeStatus{
					Addr:  pAddr,
					Alive: true,
				}
			}
		}
	}

	globalChecker = hc
	return hc
}

func (hc *HealthChecker) Start(ctx context.Context) {
	if hc == nil || len(hc.groups) == 0 {
		return
	}

	zap.S().Infof("🩺 Health checker & auto-probing started (%d proxy groups loaded)", len(hc.groups))

	for _, group := range hc.groups {
		go hc.runGroupChecker(ctx, group)
	}
}

func (hc *HealthChecker) runGroupChecker(ctx context.Context, group *ProxyGroup) {
	ticker := time.NewTicker(group.Interval)
	defer ticker.Stop()

	hc.checkGroup(group)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hc.checkGroup(group)
		}
	}
}

func (hc *HealthChecker) checkGroup(group *ProxyGroup) {
	var wg sync.WaitGroup
	for _, pAddr := range group.Proxies {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			rtt, err := pingSocks5Proxy(addr, group.URL)
			hc.mu.Lock()
			node, exists := hc.nodes[addr]
			if !exists {
				node = &ProxyNodeStatus{Addr: addr}
				hc.nodes[addr] = node
			}
			node.LastCheck = time.Now()
			if err == nil {
				node.Alive = true
				node.RTT = rtt
				node.FailCount = 0
			} else {
				node.FailCount++
				if node.FailCount >= 2 {
					node.Alive = false
				}
			}
			hc.mu.Unlock()
		}(pAddr)
	}
	wg.Wait()

	hc.updateGroupSelection(group)
}

func (hc *HealthChecker) updateGroupSelection(group *ProxyGroup) {
	group.mu.Lock()
	defer group.mu.Unlock()

	hc.mu.RLock()
	defer hc.mu.RUnlock()

	switch group.Type {
	case "url-test":
		var minRTT time.Duration = 1<<63 - 1
		bestNode := ""
		for _, pAddr := range group.Proxies {
			if node, ok := hc.nodes[pAddr]; ok && node.Alive {
				if node.RTT < minRTT {
					minRTT = node.RTT
					bestNode = pAddr
				}
			}
		}
		if bestNode != "" {
			group.Selected = bestNode
			zap.S().Debugf("[HealthCheck] Group [%s] selected optimal node: %s (latency: %v)", group.Name, bestNode, minRTT)
		}
	case "fallback":
		for _, pAddr := range group.Proxies {
			if node, ok := hc.nodes[pAddr]; ok && node.Alive {
				group.Selected = pAddr
				break
			}
		}
	}
}

func (hc *HealthChecker) SelectProxy(proxyOrGroup string) string {
	if hc == nil {
		return proxyOrGroup
	}
	hc.mu.RLock()
	group, isGroup := hc.groups[proxyOrGroup]
	hc.mu.RUnlock()

	if isGroup {
		group.mu.RLock()
		defer group.mu.RUnlock()
		if group.Selected != "" {
			return group.Selected
		}
	}
	return proxyOrGroup
}

func pingSocks5Proxy(proxyAddr string, targetURL string) (time.Duration, error) {
	u, err := url.Parse(targetURL)
	targetAddr := "cp.cloudflare.com:80"
	if err == nil && u.Hostname() != "" {
		port := u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		targetAddr = net.JoinHostPort(u.Hostname(), port)
	}

	start := time.Now()
	conn, err := dialProxyConn(proxyAddr, "tcp", targetAddr, 3*time.Second)
	if err != nil {
		return 0, err
	}
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	conn.Close()
	return time.Since(start), nil
}
