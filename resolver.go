package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// dnsCacheEntry 代表一筆 DNS 快取記錄
type dnsCacheEntry struct {
	ips       []net.IP
	expiresAt time.Time
}

type DefaultResolver struct {
	Scheme   string
	HostPort string
	Path     string
	SNI      string
	cache    sync.Map
	cacheTTL time.Duration
}

func NewDefaultResolver(rawURL string) (*DefaultResolver, error) {
	if rawURL == "" {
		rawURL = "udp://8.8.8.8:53"
	}
	if !strings.Contains(rawURL, "://") {
		rawURL = "udp://" + rawURL
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("无效的 DNS URL: %v", err)
	}

	host := u.Hostname()
	port := u.Port()

	if port == "" {
		switch u.Scheme {
		case "dot":
			port = "853"
		case "doh":
			port = "443"
		default:
			port = "53"
		}
	}

	path := u.Path
	if u.Scheme == "doh" && path == "" {
		path = "/dns-query"
	}

	resolver := &DefaultResolver{
		Scheme:   u.Scheme,
		HostPort: net.JoinHostPort(host, port),
		Path:     path,
		SNI:      u.Query().Get("sni"),
		cacheTTL: 5 * time.Minute, // 預設快取 5 分鐘
	}
	// 啟動背景定期清理過期快取 (每分鐘清理一次)
	go resolver.startCacheCleaner()
	return resolver, nil
}

// LookupIP 获取域名的真实 IP (并发支持 A 和 AAAA 记录，并包含快取)
func (r *DefaultResolver) LookupIP(domain string) ([]net.IP, error) {
	// 1. 如果是纯 IP，直接返回
	if ip := net.ParseIP(domain); ip != nil {
		return []net.IP{ip}, nil
	}

	// 2. 检查快取
	if v, ok := r.cache.Load(domain); ok {
		entry := v.(dnsCacheEntry)
		if time.Now().Before(entry.expiresAt) {
			return entry.ips, nil // 快取命中且有效
		}
		// 快取过期，继续往下执行进行真实解析
	}

	// 3. 封装通用的 DNS 内部查询闭包
	queryDNS := func(qType uint16) ([]net.IP, uint32, error) {
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(domain), qType)
		m.RecursionDesired = true

		var in *dns.Msg
		var err error

		switch r.Scheme {
		case "udp", "tcp":
			c := new(dns.Client)
			c.Net = r.Scheme
			c.Timeout = 5 * time.Second
			in, _, err = c.Exchange(m, r.HostPort)
		case "dot":
			c := new(dns.Client)
			c.Net = "tcp-tls"
			c.Timeout = 5 * time.Second
			c.TLSConfig = &tls.Config{ServerName: r.SNI}
			in, _, err = c.Exchange(m, r.HostPort)
		case "doh":
			in, err = r.exchangeDoH(m)
		default:
			return nil, 300, fmt.Errorf("不支持的 DNS 协议: %s", r.Scheme)
		}

		if err != nil {
			return nil, 300, err
		}

		var ips []net.IP
		var minTTL uint32 = 300
		for _, ans := range in.Answer {
			if qType == dns.TypeA {
				if a, ok := ans.(*dns.A); ok {
					ips = append(ips, a.A)
					if a.Hdr.Ttl > 0 && a.Hdr.Ttl < minTTL {
						minTTL = a.Hdr.Ttl
					}
				}
			} else if qType == dns.TypeAAAA {
				if aaaa, ok := ans.(*dns.AAAA); ok {
					ips = append(ips, aaaa.AAAA)
					if aaaa.Hdr.Ttl > 0 && aaaa.Hdr.Ttl < minTTL {
						minTTL = aaaa.Hdr.Ttl
					}
				}
			}
		}
		return ips, minTTL, nil
	}

	// 4. 并发发起 A 和 AAAA 查询，最大限度降低延迟
	var wg sync.WaitGroup
	var ipsV4, ipsV6 []net.IP
	var ttlV4, ttlV6 uint32 = 300, 300
	var errV4, errV6 error

	wg.Add(2)
	go func() {
		defer wg.Done()
		ipsV4, ttlV4, errV4 = queryDNS(dns.TypeA)
	}()
	go func() {
		defer wg.Done()
		ipsV6, ttlV6, errV6 = queryDNS(dns.TypeAAAA)
	}()
	wg.Wait()

	// 5. 合并查询结果 (优先保留 IPv6，符合 Happy Eyeballs 精神)
	var allIPs []net.IP
	if len(ipsV6) > 0 {
		allIPs = append(allIPs, ipsV6...)
	}
	if len(ipsV4) > 0 {
		allIPs = append(allIPs, ipsV4...)
	}

	// 如果全部失败且报错
	if len(allIPs) == 0 {
		if errV4 != nil && errV6 != nil {
			return nil, fmt.Errorf("A 和 AAAA 查询均失败, IPv4Err: %v, IPv6Err: %v", errV4, errV6)
		}
		return nil, fmt.Errorf("无 A 或 AAAA 记录返回")
	}

	// 6. 计算双端记录中的最小 TTL，确保缓存不过期出错
	minTTL := ttlV4
	if len(ipsV6) > 0 && ttlV6 < minTTL {
		minTTL = ttlV6
	}

	// 7. 写入快取
	// TTL 限制：最少 1 分钟，最多不超过设定的 r.cacheTTL (例如 5 分钟)
	ttl := time.Duration(minTTL) * time.Second
	if ttl < time.Minute {
		ttl = time.Minute
	}
	if ttl > r.cacheTTL {
		ttl = r.cacheTTL
	}

	r.cache.Store(domain, dnsCacheEntry{
		ips:       allIPs,
		expiresAt: time.Now().Add(ttl),
	})

	return allIPs, nil
}

// startCacheCleaner 背景清理過期快取
func (r *DefaultResolver) startCacheCleaner() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		r.cache.Range(func(key, value interface{}) bool {
			entry := value.(dnsCacheEntry)
			if now.After(entry.expiresAt) {
				r.cache.Delete(key)
			}
			return true // 繼續遍歷
		})
	}
}

func (r *DefaultResolver) exchangeDoH(m *dns.Msg) (*dns.Msg, error) {
	buf, err := m.Pack()
	if err != nil {
		return nil, err
	}

	reqURL := fmt.Sprintf("https://%s%s", r.HostPort, r.Path)
	req, err := http.NewRequest("POST", reqURL, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")

	if r.SNI != "" {
		req.Host = r.SNI
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{ServerName: r.SNI},
		},
		Timeout: 5 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DoH 请求失败, HTTP 状态码: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	out := new(dns.Msg)
	if err := out.Unpack(body); err != nil {
		return nil, err
	}

	return out, nil
}
