package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/zap"
)

const ShardCount = 32

type Record struct {
	IPs       []netip.Addr `json:"ips"`
	Domain    string       `json:"domain"`
	ExpiresAt time.Time    `json:"expires_at"`
}

type PersistState struct {
	V4Currents []netip.Addr       `json:"v4_currents"`
	V6Currents []netip.Addr       `json:"v6_currents"`
	Records    map[string]*Record `json:"records"`
}

type HostShard struct {
	mu sync.RWMutex
	m  map[string]*Record
}

type IPShard struct {
	mu sync.RWMutex
	m  map[netip.Addr]*Record
}

type FakeIPPool struct {
	hostShards [ShardCount]HostShard
	ipShards   [ShardCount]IPShard

	cursorMu   sync.Mutex
	v4Currents []netip.Addr
	v4Prefixes []netip.Prefix
	v6Currents []netip.Addr
	v6Prefixes []netip.Prefix

	isDirty  atomic.Bool
	ttl      time.Duration
	savePath string
}

func fnv32(key string) uint32 {
	hash := uint32(2166136261)
	const prime = 16777619
	for i := 0; i < len(key); i++ {
		hash ^= uint32(key[i])
		hash *= prime
	}
	return hash
}

func fnv32Addr(addr netip.Addr) uint32 {
	b := addr.Unmap().As16()
	hash := uint32(2166136261)
	const prime = 16777619
	for i := 0; i < len(b); i++ {
		hash ^= uint32(b[i])
		hash *= prime
	}
	return hash
}

func NewFakeIPPool(ctx context.Context, parsed *ParsedCIDRs, ttl time.Duration, savePath string) *FakeIPPool {
	p := &FakeIPPool{
		v4Prefixes: parsed.V4Prefixes,
		v6Prefixes: parsed.V6Prefixes,
		ttl:        ttl,
		savePath:   savePath,
	}

	for _, sip := range parsed.V4Starts {
		p.v4Currents = append(p.v4Currents, sip)
	}
	for _, sip := range parsed.V6Starts {
		p.v6Currents = append(p.v6Currents, sip)
	}

	for i := 0; i < ShardCount; i++ {
		p.hostShards[i].m = make(map[string]*Record)
		p.ipShards[i].m = make(map[netip.Addr]*Record)
	}

	p.loadFromFile()
	go p.startBackgroundTasks(ctx)
	return p
}

func (p *FakeIPPool) nextV4ForSubnet(idx int) netip.Addr {
	cur := p.v4Currents[idx]
	prefix := p.v4Prefixes[idx]
	for {
		cur = cur.Next().Unmap()
		if !prefix.Contains(cur) {
			cur = prefix.Addr().Unmap()
			cur = cur.Next().Unmap()
		}
		if cur == prefix.Addr().Unmap() {
			cur = cur.Next().Unmap()
		}
		p.v4Currents[idx] = cur

		ipIdx := fnv32Addr(cur) % ShardCount
		ipShard := &p.ipShards[ipIdx]
		ipShard.mu.RLock()
		_, exists := ipShard.m[cur]
		ipShard.mu.RUnlock()

		if !exists {
			break
		}
	}
	return p.v4Currents[idx]
}

func (p *FakeIPPool) nextV6ForSubnet(idx int) netip.Addr {
	cur := p.v6Currents[idx]
	prefix := p.v6Prefixes[idx]
	for {
		cur = cur.Next().Unmap()
		if !prefix.Contains(cur) {
			cur = prefix.Addr().Unmap()
			cur = cur.Next().Unmap()
		}
		if cur == prefix.Addr().Unmap() {
			cur = cur.Next().Unmap()
		}
		p.v6Currents[idx] = cur

		ipIdx := fnv32Addr(cur) % ShardCount
		ipShard := &p.ipShards[ipIdx]
		ipShard.mu.RLock()
		_, exists := ipShard.m[cur]
		ipShard.mu.RUnlock()

		if !exists {
			break
		}
	}
	return p.v6Currents[idx]
}

func (p *FakeIPPool) GetFakeIP(domain string) []netip.Addr {
	domain = strings.TrimSuffix(domain, ".")
	if GlobalAnalytics != nil {
		GlobalAnalytics.RecordDNSQuery(domain)
	}
	hIdx := fnv32(domain) % ShardCount
	hostShard := &p.hostShards[hIdx]

	hostShard.mu.Lock()
	if rec, exists := hostShard.m[domain]; exists {
		rec.ExpiresAt = time.Now().Add(p.ttl)
		p.isDirty.Store(true)
		hostShard.mu.Unlock()

		ips := make([]netip.Addr, len(rec.IPs))
		copy(ips, rec.IPs)
		return ips
	}
	hostShard.mu.Unlock()

	p.cursorMu.Lock()
	var newIPs []netip.Addr
	for i := 0; i < len(p.v4Prefixes); i++ {
		newIPs = append(newIPs, p.nextV4ForSubnet(i).Unmap())
	}
	for i := 0; i < len(p.v6Prefixes); i++ {
		newIPs = append(newIPs, p.nextV6ForSubnet(i).Unmap())
	}
	p.cursorMu.Unlock()

	rec := &Record{
		IPs:       newIPs,
		Domain:    domain,
		ExpiresAt: time.Now().Add(p.ttl),
	}

	hostShard.mu.Lock()
	hostShard.m[domain] = rec
	hostShard.mu.Unlock()

	for _, nip := range newIPs {
		nip = nip.Unmap()
		ipIdx := fnv32Addr(nip) % ShardCount
		ipShard := &p.ipShards[ipIdx]
		ipShard.mu.Lock()
		ipShard.m[nip] = rec
		ipShard.mu.Unlock()
	}

	p.isDirty.Store(true)
	p.updateMetricsTotal()
	return newIPs
}

func (p *FakeIPPool) updateMetricsTotal() {
	count := 0
	for i := 0; i < ShardCount; i++ {
		p.hostShards[i].mu.RLock()
		count += len(p.hostShards[i].m)
		p.hostShards[i].mu.RUnlock()
	}
	Metrics.FakeIPRecordsTotal.Store(int64(count))
}

func (p *FakeIPPool) GetFakeIPv4(domain string) []netip.Addr {
	all := p.GetFakeIP(domain)
	var v4s []netip.Addr
	for _, ip := range all {
		if ip.Is4() {
			v4s = append(v4s, ip)
		}
	}
	return v4s
}

func (p *FakeIPPool) GetFakeIPv6(domain string) []netip.Addr {
	all := p.GetFakeIP(domain)
	var v6s []netip.Addr
	for _, ip := range all {
		if ip.Is6() {
			v6s = append(v6s, ip)
		}
	}
	return v6s
}

func (p *FakeIPPool) LookUpAddr(addr netip.Addr) (string, bool) {
	addr = addr.Unmap()
	ipIdx := fnv32Addr(addr) % ShardCount
	ipShard := &p.ipShards[ipIdx]

	ipShard.mu.Lock()
	rec, exists := ipShard.m[addr]
	if !exists {
		ipShard.mu.Unlock()
		return "", false
	}
	rec.ExpiresAt = time.Now().Add(p.ttl)
	p.isDirty.Store(true)
	domain := rec.Domain
	ipShard.mu.Unlock()

	return domain, true
}

func (p *FakeIPPool) LookUp(ipStr string) (string, bool) {
	addr, err := netip.ParseAddr(ipStr)
	if err != nil {
		return "", false
	}
	return p.LookUpAddr(addr)
}

func (p *FakeIPPool) startBackgroundTasks(ctx context.Context) {
	cleanupTicker := time.NewTicker(1 * time.Minute)
	saveTicker := time.NewTicker(5 * time.Second)
	defer cleanupTicker.Stop()
	defer saveTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			zap.S().Infof("[FakeIP] Stopping background cleaner and persistence tasks")
			return
		case <-cleanupTicker.C:
			p.cleanExpired()
		case <-saveTicker.C:
			p.saveToFileIfDirty()
		}
	}
}

func (p *FakeIPPool) cleanExpired() {
	now := time.Now()
	count := 0

	for i := 0; i < ShardCount; i++ {
		ipShard := &p.ipShards[i]
		var expiredDomains []string

		ipShard.mu.Lock()
		for addr, rec := range ipShard.m {
			if now.After(rec.ExpiresAt) {
				delete(ipShard.m, addr)
				expiredDomains = append(expiredDomains, rec.Domain)
				count++
			}
		}
		ipShard.mu.Unlock()

		if len(expiredDomains) > 0 {
			p.hostShards[i].mu.Lock()
			for _, d := range expiredDomains {
				delete(p.hostShards[i].m, d)
			}
			p.hostShards[i].mu.Unlock()
		}
	}

	if count > 0 {
		p.isDirty.Store(true)
		zap.S().Debugf("[FakeIP] Cleaned up %d expired records", count)
	}
}

func (p *FakeIPPool) saveToFileIfDirty() {
	if !p.isDirty.Load() {
		return
	}

	allRecords := make(map[string]*Record)
	for i := 0; i < ShardCount; i++ {
		p.hostShards[i].mu.RLock()
		for k, v := range p.hostShards[i].m {
			allRecords[k] = v
		}
		p.hostShards[i].mu.RUnlock()
	}

	Metrics.FakeIPRecordsTotal.Store(int64(len(allRecords)))

	p.cursorMu.Lock()
	v4Copy := make([]netip.Addr, len(p.v4Currents))
	copy(v4Copy, p.v4Currents)
	v6Copy := make([]netip.Addr, len(p.v6Currents))
	copy(v6Copy, p.v6Currents)
	p.cursorMu.Unlock()

	state := PersistState{
		V4Currents: v4Copy,
		V6Currents: v6Copy,
		Records:    allRecords,
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	p.isDirty.Store(false)

	tempFile := p.savePath + ".tmp"
	if err := os.WriteFile(tempFile, data, 0644); err == nil {
		os.Rename(tempFile, p.savePath)
		zap.S().Debugf("[FakeIP] State changed, flushed to %s", p.savePath)
	}
}

func (p *FakeIPPool) loadFromFile() {
	data, err := os.ReadFile(p.savePath)
	if err != nil {
		zap.S().Infof("[FakeIP] No persistence file found, initializing new pool")
		return
	}
	var state PersistState
	if err := json.Unmarshal(data, &state); err != nil {
		zap.S().Errorf("[FakeIP] Failed to read persistence file: %v", err)
		return
	}

	if len(state.V4Currents) == len(p.v4Prefixes) {
		for i, cur := range state.V4Currents {
			if cur.IsValid() && p.v4Prefixes[i].Contains(cur) {
				p.v4Currents[i] = cur.Unmap()
			}
		}
	}
	if len(state.V6Currents) == len(p.v6Prefixes) {
		for i, cur := range state.V6Currents {
			if cur.IsValid() && p.v6Prefixes[i].Contains(cur) {
				p.v6Currents[i] = cur.Unmap()
			}
		}
	}

	now := time.Now()
	validCount := 0
	for domain, rec := range state.Records {
		if now.After(rec.ExpiresAt) {
			continue
		}

		var validIPs []netip.Addr
		for _, addr := range rec.IPs {
			addr = addr.Unmap()
			inPrefix := false
			for _, pfx := range p.v4Prefixes {
				if pfx.Contains(addr) {
					inPrefix = true
					break
				}
			}
			if !inPrefix {
				for _, pfx := range p.v6Prefixes {
					if pfx.Contains(addr) {
						inPrefix = true
						break
					}
				}
			}
			if inPrefix {
				validIPs = append(validIPs, addr)
			}
		}
		if len(validIPs) == 0 {
			continue
		}
		rec.IPs = validIPs

		hIdx := fnv32(domain) % ShardCount
		hostShard := &p.hostShards[hIdx]
		hostShard.m[domain] = rec

		for _, addr := range rec.IPs {
			ipIdx := fnv32Addr(addr) % ShardCount
			p.ipShards[ipIdx].m[addr] = rec
		}
		validCount++
	}
	Metrics.FakeIPRecordsTotal.Store(int64(validCount))
	zap.S().Infof("[FakeIP] Restored %d valid records from %s", validCount, p.savePath)
}

func (p *FakeIPPool) Close() {
	p.isDirty.Store(true)
	p.saveToFileIfDirty()
}

func (p *FakeIPPool) Clear() {
	if p == nil {
		return
	}
	for i := 0; i < ShardCount; i++ {
		p.hostShards[i].mu.Lock()
		p.hostShards[i].m = make(map[string]*Record)
		p.hostShards[i].mu.Unlock()

		p.ipShards[i].mu.Lock()
		p.ipShards[i].m = make(map[netip.Addr]*Record)
		p.ipShards[i].mu.Unlock()
	}
	Metrics.FakeIPRecordsTotal.Store(0)
	p.isDirty.Store(true)
	p.saveToFileIfDirty()
	zap.S().Infof("[FakeIP] Pool records cleared via WebUI control")
}

func startDNSServer(ctx context.Context, addr string) {
	dns.HandleFunc(".", func(w dns.ResponseWriter, r *dns.Msg) {
		Metrics.DNSQueriesTotal.Add(1)
		m := new(dns.Msg)
		m.SetReply(r)

		clientIP := w.RemoteAddr().String()

		for _, q := range r.Question {
			qTypeStr := dns.TypeToString[q.Qtype]
			switch q.Qtype {
			case dns.TypeA:
				fakeIPs := pool.GetFakeIPv4(q.Name)
				rand.Shuffle(len(fakeIPs), func(i, j int) {
					fakeIPs[i], fakeIPs[j] = fakeIPs[j], fakeIPs[i]
				})
				for _, fakeIP := range fakeIPs {
					rr, _ := dns.NewRR(fmt.Sprintf("%s A %s", q.Name, fakeIP.String()))
					if rr != nil {
						m.Answer = append(m.Answer, rr)
					}
				}
				zap.S().Debugf("[FakeIP DNS] Query from %s: '%s' [%s] -> Answer: %v", clientIP, q.Name, qTypeStr, fakeIPs)

			case dns.TypeAAAA:
				fakeIPs := pool.GetFakeIPv6(q.Name)
				rand.Shuffle(len(fakeIPs), func(i, j int) {
					fakeIPs[i], fakeIPs[j] = fakeIPs[j], fakeIPs[i]
				})
				for _, fakeIP := range fakeIPs {
					rr, _ := dns.NewRR(fmt.Sprintf("%s AAAA %s", q.Name, fakeIP.String()))
					if rr != nil {
						m.Answer = append(m.Answer, rr)
					}
				}
				zap.S().Debugf("[FakeIP DNS] Query from %s: '%s' [%s] -> Answer: %v", clientIP, q.Name, qTypeStr, fakeIPs)

			default:
				zap.S().Debugf("[FakeIP DNS] Query from %s: '%s' [%s] -> Answer: NOERROR (empty)", clientIP, q.Name, qTypeStr)
			}
		}
		w.WriteMsg(m)
	})

	udpServer := &dns.Server{Addr: addr, Net: "udp"}
	tcpServer := &dns.Server{Addr: addr, Net: "tcp"}
	zap.S().Infof("FakeIP DNS server started -> [udp://%s] and [tcp://%s]", addr, addr)

	go func() {
		<-ctx.Done()
		zap.S().Infof("[DNS] Shutting down FakeIP DNS server...")
		udpServer.ShutdownContext(context.Background())
		tcpServer.ShutdownContext(context.Background())
	}()

	go func() {
		if err := tcpServer.ListenAndServe(); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			zap.S().Errorf("TCP DNS server fatal error: %v", err)
		}
	}()

	go func() {
		if err := udpServer.ListenAndServe(); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			zap.S().Errorf("UDP DNS server fatal error: %v", err)
		}
	}()
}
