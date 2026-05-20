package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/zap"
)

// 性能优化：分片锁数量定义
const ShardCount = 64

type Record struct {
	IPs       []string  `json:"ips"` // 一个站点同时持有并绑定多个网段的 FakeIP
	Domain    string    `json:"domain"`
	ExpiresAt time.Time `json:"expires_at"`
}

type PersistState struct {
	Currents [][]byte           `json:"currents"` // 持久化保存多个网段的各自游标位置
	Records  map[string]*Record `json:"records"`  // 改由以 domain 为键进行持久化，防止数据重复
}

type HostShard struct {
	mu sync.RWMutex
	m  map[string]*Record
}

type IPShard struct {
	mu sync.RWMutex
	m  map[string]*Record
}

type FakeIPPool struct {
	hostShards [ShardCount]HostShard
	ipShards   [ShardCount]IPShard

	cursorMu sync.Mutex
	currents []net.IP     // 多网段独立递增游标
	ipnets   []*net.IPNet // 多网段范畴保护

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

func fnv32Bytes(key []byte) uint32 {
	hash := uint32(2166136261)
	const prime = 16777619
	for i := 0; i < len(key); i++ {
		hash ^= uint32(key[i])
		hash *= prime
	}
	return hash
}

func NewFakeIPPool(ctx context.Context, startIPs []net.IP, ipnets []*net.IPNet, ttl time.Duration, savePath string) *FakeIPPool {
	pool := &FakeIPPool{
		ipnets:   ipnets,
		ttl:      ttl,
		savePath: savePath,
	}

	for _, sip := range startIPs {
		pool.currents = append(pool.currents, cloneIP(sip))
	}

	for i := 0; i < ShardCount; i++ {
		pool.hostShards[i].m = make(map[string]*Record)
		pool.ipShards[i].m = make(map[string]*Record)
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

// nextIPForSubnet 在指定索引的 Overlay 网段内捞出一个干净未分配的 IP
func (p *FakeIPPool) nextIPForSubnet(idx int) net.IP {
	for {
		incIP(p.currents[idx])
		if !p.ipnets[idx].Contains(p.currents[idx]) {
			zap.S().Warnf("[FakeIP] CIDR 网段 %s 已耗尽，重置游标", p.ipnets[idx].String())
			p.currents[idx] = cloneIP(p.ipnets[idx].IP)
			incIP(p.currents[idx])
		}

		ipIdx := fnv32Bytes(p.currents[idx]) % ShardCount
		shard := &p.ipShards[ipIdx]

		shard.mu.RLock()
		_, exists := shard.m[string(p.currents[idx])]
		shard.mu.RUnlock()

		if !exists {
			break
		}
	}
	return cloneIP(p.currents[idx])
}

// GetFakeIP 一次性返回全量配置网段对应的多个 FakeIP
func (p *FakeIPPool) GetFakeIP(domain string) []net.IP {
	domain = strings.TrimSuffix(domain, ".")
	hIdx := fnv32(domain) % ShardCount
	hostShard := &p.hostShards[hIdx]

	// 1. 缓存命中，直接解析并返回已绑定的全量 IP 组合
	hostShard.mu.Lock()
	if rec, exists := hostShard.m[domain]; exists {
		rec.ExpiresAt = time.Now().Add(p.ttl)
		p.isDirty.Store(true)
		hostShard.mu.Unlock()

		var ips []net.IP
		for _, ipStr := range rec.IPs {
			ips = append(ips, net.ParseIP(ipStr))
		}
		return ips
	}
	hostShard.mu.Unlock()

	// 2. 缓存未命中，在每个指定的子网段中各提取一个可用 IP
	p.cursorMu.Lock()
	var newIPs []net.IP
	var newIPStrs []string
	for i := 0; i < len(p.ipnets); i++ {
		nip := p.nextIPForSubnet(i)
		newIPs = append(newIPs, nip)
		newIPStrs = append(newIPStrs, nip.String())
	}
	p.cursorMu.Unlock()

	rec := &Record{
		IPs:       newIPStrs,
		Domain:    domain,
		ExpiresAt: time.Now().Add(p.ttl),
	}

	// 3. 将新映射关系同步至各自的分片槽中
	hostShard.mu.Lock()
	hostShard.m[domain] = rec
	hostShard.mu.Unlock()

	for _, ipStr := range newIPStrs {
		ipIdx := fnv32(ipStr) % ShardCount
		ipShard := &p.ipShards[ipIdx]
		ipShard.mu.Lock()
		ipShard.m[ipStr] = rec
		ipShard.mu.Unlock()
	}

	p.isDirty.Store(true)
	zap.S().Debugf("[FakeIP] 站点 %s 成功映射至多组分流 IP: %v", domain, newIPStrs)
	return newIPs
}

func (p *FakeIPPool) LookUp(ipStr string) (string, bool) {
	ipIdx := fnv32(ipStr) % ShardCount
	ipShard := &p.ipShards[ipIdx]

	ipShard.mu.Lock()
	rec, exists := ipShard.m[ipStr]
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
	now := time.Now()
	count := 0

	for i := 0; i < ShardCount; i++ {
		ipShard := &p.ipShards[i]
		ipShard.mu.Lock()
		for ipStr, rec := range ipShard.m {
			if now.After(rec.ExpiresAt) {
				delete(ipShard.m, ipStr)

				hIdx := fnv32(rec.Domain) % ShardCount
				hostShard := &p.hostShards[hIdx]
				hostShard.mu.Lock()
				delete(hostShard.m, rec.Domain)
				hostShard.mu.Unlock()

				p.isDirty.Store(true)
				count++
			}
		}
		ipShard.mu.Unlock()
	}

	if count > 0 {
		zap.S().Debugf("[FakeIP] 清理了 %d 条过期记录", count)
	}
}

func (p *FakeIPPool) saveToFileIfDirty() {
	if !p.isDirty.Load() {
		return
	}

	allRecords := make(map[string]*Record)
	for i := 0; i < ShardCount; i++ {
		hostShard := &p.hostShards[i]
		hostShard.mu.RLock()
		for k, v := range hostShard.m {
			allRecords[k] = v
		}
		hostShard.mu.RUnlock()
	}

	p.cursorMu.Lock()
	var currentsCopy [][]byte
	for _, cur := range p.currents {
		currentsCopy = append(currentsCopy, []byte(cloneIP(cur)))
	}
	p.cursorMu.Unlock()

	state := PersistState{
		Currents: currentsCopy,
		Records:  allRecords,
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	p.isDirty.Store(false)

	tempFile := p.savePath + ".tmp"
	if err := os.WriteFile(tempFile, data, 0644); err == nil {
		os.Rename(tempFile, p.savePath)
		zap.S().Debugf("[FakeIP] 内存数据发生变更，已异步落盘至 %s", p.savePath)
	}
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

	if len(state.Currents) == len(p.ipnets) {
		for i, curBytes := range state.Currents {
			recoveredCurrent := net.IP(curBytes)
			if len(recoveredCurrent) == 16 && p.ipnets[i].Contains(recoveredCurrent) {
				p.currents[i] = cloneIP(recoveredCurrent)
			}
		}
	}

	now := time.Now()
	validCount := 0
	for domain, rec := range state.Records {
		if now.After(rec.ExpiresAt) {
			continue
		}

		hIdx := fnv32(domain) % ShardCount
		hostShard := &p.hostShards[hIdx]
		hostShard.m[domain] = rec

		for _, ipStr := range rec.IPs {
			ipIdx := fnv32(ipStr) % ShardCount
			p.ipShards[ipIdx].m[ipStr] = rec
		}
		validCount++
	}
	zap.S().Infof("[FakeIP] 成功从 %s 恢复了 %d 条有效记录", p.savePath, validCount)
}

func (p *FakeIPPool) Close() {
	p.isDirty.Store(true)
	p.saveToFileIfDirty()
}

func startDNSServer(ctx context.Context, addr string) {
	dns.HandleFunc(".", func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		for _, q := range r.Question {
			if q.Qtype == dns.TypeAAAA {
				fakeIPs := pool.GetFakeIP(q.Name)

				// 对返回的 FakeIP 数组执行洗牌算法（Shuffle）打散顺序
				// 由于 GetFakeIP 每次命中都会生成一个全新的切片，因此直接在原数组上无污染打散即可
				rand.Shuffle(len(fakeIPs), func(i, j int) {
					fakeIPs[i], fakeIPs[j] = fakeIPs[j], fakeIPs[i]
				})

				// 依次打包写入 Answer 队列中传回
				for _, fakeIP := range fakeIPs {
					rr, _ := dns.NewRR(fmt.Sprintf("%s AAAA %s", q.Name, fakeIP.String()))
					if rr != nil {
						m.Answer = append(m.Answer, rr)
					}
				}
			}
		}
		w.WriteMsg(m)
	})

	udpServer := &dns.Server{Addr: addr, Net: "udp"}
	tcpServer := &dns.Server{Addr: addr, Net: "tcp"}
	zap.S().Infof("FakeIP DNS 启动完成 -> [udp://%s] 和 [tcp://%s]", addr, addr)

	go func() {
		<-ctx.Done()
		zap.S().Infof("[DNS] 正在关闭 FakeIP DNS 服务器...")
		udpServer.ShutdownContext(context.Background())
		tcpServer.ShutdownContext(context.Background())
	}()

	go func() {
		if err := tcpServer.ListenAndServe(); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			zap.S().Errorf("TCP DNS 服务器发生致命故障退出: %v", err)
		}
	}()

	go func() {
		if err := udpServer.ListenAndServe(); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			zap.S().Errorf("UDP DNS 服务器发生致命故障退出: %v", err)
		}
	}()
}
