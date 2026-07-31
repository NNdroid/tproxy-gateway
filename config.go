package main

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

var (
	ActiveConfigPath string
	configMu         sync.Mutex
)

type Config struct {
	Log           LogConfig          `yaml:"log"`
	Metrics       MetricsConfig      `yaml:"metrics"`
	UI            UIConfig           `yaml:"ui"`
	Server        ServerConfig       `yaml:"server"`
	Routing       RoutingConfig      `yaml:"routing"`
	FakeIP        FakeIPConfig       `yaml:"fake_ip"`
	AdBlock       AdBlockConfig      `yaml:"adblock"`
	Subscriptions []SubConfig        `yaml:"subscriptions"`
	ProxyGroups   []ProxyGroupConfig `yaml:"proxy_groups"`
	Rules         []RuleConfig       `yaml:"rules"`
}

type LogConfig struct {
	Level string `yaml:"level"`
}

type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Addr    string `yaml:"addr"`
}

type UIConfig struct {
	Enabled bool   `yaml:"enabled"`
	Mode    string `yaml:"mode"` // "port" | "domain" | "both"
	Addr    string `yaml:"addr"`
	Domain  string `yaml:"domain"`
	Secret  string `yaml:"secret"`
}

type ServerConfig struct {
	DNSAddr    string `yaml:"dns_addr"`
	TProxyAddr string `yaml:"tproxy_addr"`
	SocksAddr  string `yaml:"socks_addr"`
	HTTPAddr   string `yaml:"http_addr"`
}

type RoutingConfig struct {
	DefaultUpstream string `yaml:"default_upstream"`
	DefaultDNS      string `yaml:"default_dns"`
	Fwmark          int    `yaml:"fwmark"`
	Table           int    `yaml:"table"`
	NftTable        string `yaml:"nft_table"`
	AutoRoute       bool   `yaml:"auto_route"`
}

type FakeIPConfig struct {
	CIDRs       []string `yaml:"cidrs"`
	TTL         string   `yaml:"ttl"`
	PersistFile string   `yaml:"persist_file"`
}

type AdBlockConfig struct {
	Enabled bool     `yaml:"enabled"`
	URLs    []string `yaml:"urls"`
	Files   []string `yaml:"files"`
}

type SubConfig struct {
	Name     string `yaml:"name"`
	URL      string `yaml:"url"`
	Interval string `yaml:"interval"`
}

type ProxyGroupConfig struct {
	Name     string   `yaml:"name"`
	Type     string   `yaml:"type"`
	Proxies  []string `yaml:"proxies"`
	URL      string   `yaml:"url"`
	Interval string   `yaml:"interval"`
}

type RuleConfig struct {
	Type          string            `yaml:"type"`
	Payload       string            `yaml:"payload"`
	Proxy         string            `yaml:"proxy"`
	Domains       []string          `yaml:"domains"`
	HeaderRewrite map[string]string `yaml:"header_rewrite"`
}

type ParsedCIDRs struct {
	V4Starts   []netip.Addr
	V4Prefixes []netip.Prefix
	V6Starts   []netip.Addr
	V6Prefixes []netip.Prefix
}

func LoadConfig(path string) (*Config, error) {
	configMu.Lock()
	defer configMu.Unlock()

	ActiveConfigPath = path
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %v", err)
	}

	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.Metrics.Addr == "" {
		cfg.Metrics.Addr = ":9090"
	}
	if cfg.Server.DNSAddr == "" {
		cfg.Server.DNSAddr = ":5353"
	}
	if cfg.Server.TProxyAddr == "" {
		cfg.Server.TProxyAddr = "[::]:10800"
	}
	if len(cfg.FakeIP.CIDRs) == 0 {
		cfg.FakeIP.CIDRs = []string{"198.18.0.0/15", "fd00::/8"}
	}
	if cfg.Routing.DefaultUpstream == "" {
		cfg.Routing.DefaultUpstream = "DIRECT"
	}

	if cfg.Routing.Fwmark == 0 {
		cfg.Routing.Fwmark = 1
	}
	if cfg.Routing.Table == 0 {
		cfg.Routing.Table = 1
	}
	if cfg.Routing.NftTable == "" {
		cfg.Routing.NftTable = "tproxy_gw"
	}

	return &cfg, nil
}

func SaveAndValidateConfig(path string, rawYAML []byte) (*Config, error) {
	var tmpConfig Config
	if err := yaml.Unmarshal(rawYAML, &tmpConfig); err != nil {
		return nil, fmt.Errorf("YAML syntax error: %v", err)
	}

	if _, err := tmpConfig.FakeIP.ParseCIDRs(); err != nil {
		return nil, fmt.Errorf("invalid FakeIP CIDR configuration: %v", err)
	}

	configMu.Lock()
	defer configMu.Unlock()

	tempFile := path + ".tmp"
	if err := os.WriteFile(tempFile, rawYAML, 0644); err != nil {
		return nil, fmt.Errorf("failed to write temporary config file: %v", err)
	}

	if err := os.Rename(tempFile, path); err != nil {
		return nil, fmt.Errorf("failed to replace config file: %v", err)
	}

	return &tmpConfig, nil
}

func (c *FakeIPConfig) ParseCIDRs() (*ParsedCIDRs, error) {
	parsed := &ParsedCIDRs{}
	for _, cidr := range c.CIDRs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse CIDR %s: %v", cidr, err)
		}
		addr := prefix.Addr()
		if addr.Is4() {
			parsed.V4Starts = append(parsed.V4Starts, addr)
			parsed.V4Prefixes = append(parsed.V4Prefixes, prefix)
		} else if addr.Is6() {
			parsed.V6Starts = append(parsed.V6Starts, addr)
			parsed.V6Prefixes = append(parsed.V6Prefixes, prefix)
		}
	}
	if len(parsed.V4Prefixes) == 0 && len(parsed.V6Prefixes) == 0 {
		return nil, fmt.Errorf("at least one valid FakeIP CIDR range must be configured")
	}
	return parsed, nil
}

func parseSocksAddr(rawAddr string) (user, pass, addr string) {
	if !strings.Contains(rawAddr, "@") {
		return "", "", rawAddr
	}
	parts := strings.SplitN(rawAddr, "@", 2)
	userInfo := parts[0]
	addr = parts[1]

	if strings.Contains(userInfo, ":") {
		up := strings.SplitN(userInfo, ":", 2)
		user = up[0]
		pass = up[1]
	} else {
		user = userInfo
	}
	return
}
