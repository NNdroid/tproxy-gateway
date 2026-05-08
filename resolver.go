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
	"time"

	"github.com/miekg/dns"
)

type DefaultResolver struct {
	Scheme   string
	HostPort string
	Path     string
	SNI      string
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

	return &DefaultResolver{
		Scheme:   u.Scheme,
		HostPort: net.JoinHostPort(host, port),
		Path:     path,
		SNI:      u.Query().Get("sni"),
	}, nil
}

func (r *DefaultResolver) LookupIP(domain string) ([]net.IP, error) {
	if ip := net.ParseIP(domain); ip != nil {
		return []net.IP{ip}, nil
	}

	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(domain), dns.TypeA)
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
		return nil, fmt.Errorf("不支持的 DNS 协议: %s", r.Scheme)
	}

	if err != nil {
		return nil, err
	}

	var ips []net.IP
	for _, ans := range in.Answer {
		if a, ok := ans.(*dns.A); ok {
			ips = append(ips, a.A)
		}
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("无 A 记录返回")
	}
	return ips, nil
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