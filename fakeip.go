package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/zap"
)

type Record struct {
	IP        string    `json:"ip"`
	Domain    string    `json:"domain"`
	ExpiresAt time.Time `json:"expires_at"`
}

type PersistState struct {
	Current []byte             `json:"current"`
	Records map[string]*Record `json:"records"`
}

type FakeIPPool struct {
	mu       sync.RWMutex
	ip2rec   map[string]*Record
	host2rec map[string]*Record
	current  net.IP
	ipnet    *net.IPNet
	isDirty  bool
	ttl      time.Duration
	savePath string
}

func NewFakeIPPool(ctx context.Context, startIP net.IP, ipnet *net.IPNet, ttl time.Duration, savePath string) *FakeIPPool {
	pool := &FakeIPPool{
		ip2rec:   make(map[string]*Record),
		host2rec: make(map[string]*Record),
		current:  cloneIP(startIP),
		ipnet:    ipnet,
		ttl:      ttl,
		savePath: savePath,
	}
	pool.loadFromFile()
	go pool.startBackgroundTasks(ctx)
	return pool
}

func cloneIP(ip net.IP) net.IP {
	dup := make(net.IP, len(ip))
	copy(dup, ip)
	return dup
}

func incIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func (p *FakeIPPool) nextIP() net.IP {
	for {
		incIP(p.current)
		if !p.ipnet.Contains(p.current) {
			zap.S().Warnf("[FakeIP] CIDR 网段 %s 已耗尽，重置游标", p.ipnet.String())
			p.current = cloneIP(p.ipnet.IP)
			incIP(p.current)
		}
		if _, exists := p.ip2rec[p.current.String()]; !exists {
			break
		}
	}
	return cloneIP(p.current)
}

func (p *FakeIPPool) GetFakeIP(domain string) net.IP {
	domain = strings.TrimSuffix(domain, ".")
	p.mu.Lock()
	defer p.mu.Unlock()

	if rec, exists := p.host2rec[domain]; exists {
		rec.ExpiresAt = time.Now().Add(p.ttl)
		p.isDirty = true
		return net.ParseIP(rec.IP)
	}

	newIP := p.nextIP()
	newIPStr := newIP.String()

	rec := &Record{
		IP:        newIPStr,
		Domain:    domain,
		ExpiresAt: time.Now().Add(p.ttl),
	}

	p.host2rec[domain] = rec
	p.ip2rec[newIPStr] = rec
	p.isDirty = true

	zap.S().Debugf("[FakeIP] 分配新 IP: %s -> %s", domain, newIPStr)
	return newIP
}

func (p *FakeIPPool) LookUp(ipStr string) (string, bool) {
	p.mu.RLock()
	rec, exists := p.ip2rec[ipStr]
	p.mu.RUnlock()

	if !exists {
		return "", false
	}

	p.mu.Lock()
	rec.ExpiresAt = time.Now().Add(p.ttl)
	p.isDirty = true
	p.mu.Unlock()

	return rec.Domain, true
}

func (p *FakeIPPool) startBackgroundTasks(ctx context.Context) {
	cleanupTicker := time.NewTicker(1 * time.Minute)
	saveTicker := time.NewTicker(5 * time.Second)
	defer cleanupTicker.Stop()
	defer saveTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			zap.S().Infof("[FakeIP] 停止后台清理与持久化任务")
			return
		case <-cleanupTicker.C:
			p.cleanExpired()
		case <-saveTicker.C:
			p.saveToFileIfDirty()
		}
	}
}

func (p *FakeIPPool) cleanExpired() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	count := 0
	for ipStr, rec := range p.ip2rec {
		if now.After(rec.ExpiresAt) {
			delete(p.ip2rec, ipStr)
			delete(p.host2rec, rec.Domain)
			p.isDirty = true
			count++
		}
	}
	if count > 0 {
		zap.S().Debugf("[FakeIP] 清理了 %d 条过期记录", count)
	}
}

func (p *FakeIPPool) saveToFileIfDirty() {
	p.mu.Lock()
	if !p.isDirty {
		p.mu.Unlock()
		return
	}

	state := PersistState{
		Current: []byte(p.current),
		Records: p.ip2rec,
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	p.isDirty = false
	p.mu.Unlock()

	tempFile := p.savePath + ".tmp"
	os.WriteFile(tempFile, data, 0644)
	os.Rename(tempFile, p.savePath)
	zap.S().Debugf("[FakeIP] 内存数据发生变更，已异步落盘至 %s", p.savePath)
}

func (p *FakeIPPool) loadFromFile() {
	data, err := os.ReadFile(p.savePath)
	if err != nil {
		zap.S().Infof("[FakeIP] 无历史持久化文件，使用全新池")
		return
	}
	var state PersistState
	if json.Unmarshal(data, &state) != nil {
		zap.S().Errorf("[FakeIP] 读取历史数据失败，可能文件损坏")
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	recoveredCurrent := net.IP(state.Current)
	if len(recoveredCurrent) == 16 && p.ipnet.Contains(recoveredCurrent) {
		p.current = cloneIP(recoveredCurrent)
	}

	now := time.Now()
	validCount := 0
	for _, rec := range state.Records {
		if now.After(rec.ExpiresAt) || !p.ipnet.Contains(net.ParseIP(rec.IP)) {
			continue
		}
		p.ip2rec[rec.IP] = rec
		p.host2rec[rec.Domain] = rec
		validCount++
	}
	zap.S().Infof("[FakeIP] 成功从 %s 恢复了 %d 条有效记录", p.savePath, validCount)
}

func (p *FakeIPPool) Close() {
	p.mu.Lock()
	p.isDirty = true
	p.mu.Unlock()
	p.saveToFileIfDirty()
}

func startDNSServer(ctx context.Context, addr string) {
	dns.HandleFunc(".", func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		for _, q := range r.Question {
			if q.Qtype == dns.TypeAAAA {
				fakeIP := pool.GetFakeIP(q.Name)
				rr, _ := dns.NewRR(fmt.Sprintf("%s AAAA %s", q.Name, fakeIP.String()))
				if rr != nil {
					m.Answer = append(m.Answer, rr)
				}
			}
		}
		w.WriteMsg(m)
	})

	server := &dns.Server{Addr: addr, Net: "udp"}
	zap.S().Infof("FakeIP DNS 启动于 udp://%s", addr)

	go func() {
		<-ctx.Done()
		zap.S().Infof("[DNS] 正在关闭 FakeIP DNS 服务器...")
		server.ShutdownContext(context.Background())
	}()

	server.ListenAndServe()
}
