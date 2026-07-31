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
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

type dnsCacheEntry struct {
	ips       []net.IP
	expiresAt time.Time
	staleAt   time.Time
}

type DefaultResolver struct {
	Scheme     string
	HostPort   string
	Path       string
	SNI        string
	cache      sync.Map
	cacheTTL   time.Duration
	httpClient *http.Client
	dnsClient  *dns.Client
	sfGroup    singleflight.Group
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
		return nil, fmt.Errorf("invalid DNS URL: %v", err)
	}

	host := u.Hostname()
	port := u.Port()

	if port == "" {
		switch u.Scheme {
		case "dot", "doq":
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
		cacheTTL: 5 * time.Minute,
	}

	if resolver.Scheme == "doh" {
		resolver.httpClient = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig:     &tls.Config{ServerName: resolver.SNI},
				MaxIdleConns:        100,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 5 * time.Second,
			},
			Timeout: 5 * time.Second,
		}
	} else if resolver.Scheme == "dot" || resolver.Scheme == "tcp" || resolver.Scheme == "udp" || resolver.Scheme == "doq" {
		resolver.dnsClient = &dns.Client{
			Net:     resolver.Scheme,
			Timeout: 5 * time.Second,
		}
		if resolver.Scheme == "dot" || resolver.Scheme == "doq" {
			resolver.dnsClient.Net = "tcp-tls"
			resolver.dnsClient.TLSConfig = &tls.Config{ServerName: resolver.SNI}
		}
	}

	go resolver.startCacheCleaner()
	return resolver, nil
}

func (r *DefaultResolver) LookupIP(domain string) ([]net.IP, error) {
	if ip := net.ParseIP(domain); ip != nil {
		return []net.IP{ip}, nil
	}

	if globalAdBlocker != nil && globalAdBlocker.IsBlocked(domain) {
		zap.S().Debugf("[DNS] Domain '%s' blocked by AdBlock filter", domain)
		return nil, fmt.Errorf("domain blocked by AdBlock filter: %s", domain)
	}

	now := time.Now()

	if v, ok := r.cache.Load(domain); ok {
		entry := v.(dnsCacheEntry)
		if now.Before(entry.expiresAt) {
			zap.S().Debugf("[DNS] Fresh cache hit for '%s' -> %v", domain, entry.ips)
			return entry.ips, nil
		}
		if now.Before(entry.staleAt) {
			zap.S().Debugf("[DNS] SWR cache hit for '%s' -> %v (stale, refreshing in background)", domain, entry.ips)
			go func(d string) {
				_, _, _ = r.singleflightLookup(d)
			}(domain)
			return entry.ips, nil
		}
	}

	zap.S().Debugf("[DNS] Cache miss for '%s', querying upstream [%s://%s]...", domain, r.Scheme, r.HostPort)
	ips, _, err := r.singleflightLookup(domain)
	return ips, err
}

func (r *DefaultResolver) singleflightLookup(domain string) ([]net.IP, time.Duration, error) {
	res, err, shared := r.sfGroup.Do(domain, func() (interface{}, error) {
		return r.rawQueryDNS(domain)
	})

	if shared {
		zap.S().Debugf("[DNS] SingleFlight coalesced concurrent query for '%s'", domain)
	}

	if err != nil {
		return nil, 0, err
	}

	ips := res.([]net.IP)
	dup := make([]net.IP, len(ips))
	copy(dup, ips)
	return dup, r.cacheTTL, nil
}

func (r *DefaultResolver) rawQueryDNS(domain string) ([]net.IP, error) {
	queryDNS := func(qType uint16) ([]net.IP, uint32, error) {
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(domain), qType)
		m.RecursionDesired = true

		var in *dns.Msg
		var err error

		switch r.Scheme {
		case "udp", "tcp", "dot", "doq":
			in, _, err = r.dnsClient.Exchange(m, r.HostPort)
		case "doh":
			in, err = r.exchangeDoH(m)
		default:
			return nil, 300, fmt.Errorf("unsupported DNS protocol: %s", r.Scheme)
		}

		if err != nil {
			zap.S().Warnf("[DNS] Upstream DNS [%s://%s] failed for '%s': %v, falling back to UDP 223.5.5.5:53", r.Scheme, r.HostPort, domain, err)
			fallbackClient := &dns.Client{Net: "udp", Timeout: 2 * time.Second}
			in, _, err = fallbackClient.Exchange(m, "223.5.5.5:53")
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

	var allIPs []net.IP
	if len(ipsV6) > 0 {
		allIPs = append(allIPs, ipsV6...)
	}
	if len(ipsV4) > 0 {
		allIPs = append(allIPs, ipsV4...)
	}

	if len(allIPs) == 0 {
		if errV4 != nil && errV6 != nil {
			return nil, fmt.Errorf("both A and AAAA queries failed, IPv4Err: %v, IPv6Err: %v", errV4, errV6)
		}
		return nil, fmt.Errorf("no A or AAAA records returned")
	}

	minTTL := ttlV4
	if len(ipsV6) > 0 && ttlV6 < minTTL {
		minTTL = ttlV6
	}

	ttl := time.Duration(minTTL) * time.Second
	if ttl < time.Minute {
		ttl = time.Minute
	}
	if ttl > r.cacheTTL {
		ttl = r.cacheTTL
	}

	now := time.Now()
	r.cache.Store(domain, dnsCacheEntry{
		ips:       allIPs,
		expiresAt: now.Add(ttl),
		staleAt:   now.Add(ttl + 10*time.Minute),
	})

	zap.S().Debugf("[DNS] Raw query '%s' success -> IPs: %v (TTL: %v)", domain, allIPs, ttl)
	return allIPs, nil
}

func (r *DefaultResolver) startCacheCleaner() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		count := 0
		r.cache.Range(func(key, value interface{}) bool {
			entry := value.(dnsCacheEntry)
			if now.After(entry.staleAt) {
				r.cache.Delete(key)
				count++
			}
			return true
		})
		if count > 0 {
			zap.S().Debugf("[DNS] Cleaned up %d stale DNS cache entries", count)
		}
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

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DoH request failed, HTTP status code: %d", resp.StatusCode)
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

func (r *DefaultResolver) ClearCache() {
	if r == nil {
		return
	}
	r.cache.Range(func(key, value interface{}) bool {
		r.cache.Delete(key)
		return true
	})
	zap.S().Infof("[DNS] Cache cleared via WebUI control")
}

func (r *DefaultResolver) CacheCount() int {
	if r == nil {
		return 0
	}
	count := 0
	now := time.Now()
	r.cache.Range(func(key, value interface{}) bool {
		if entry, ok := value.(dnsCacheEntry); ok {
			if now.Before(entry.staleAt) {
				count++
			}
		}
		return true
	})
	return count
}
