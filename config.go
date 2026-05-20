package main

import (
	"fmt"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Log     LogConfig     `yaml:"log"`
	Server  ServerConfig  `yaml:"server"`
	Routing RoutingConfig `yaml:"routing"`
	FakeIP  FakeIPConfig  `yaml:"fake_ip"`
	Rules   []RuleConfig  `yaml:"rules"`
}

type LogConfig struct {
	Level string `yaml:"level"`
}

type ServerConfig struct {
	DNSAddr    string `yaml:"dns_addr"`
	TProxyAddr string `yaml:"tproxy_addr"`
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

type RuleConfig struct {
	Proxy         string            `yaml:"proxy"`
	Domains       []string          `yaml:"domains"`
	HeaderRewrite map[string]string `yaml:"header_rewrite"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("讀取配置文件失敗: %v", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 YAML 失敗: %v", err)
	}

	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.Server.DNSAddr == "" {
		cfg.Server.DNSAddr = ":5353"
	}
	if cfg.Server.TProxyAddr == "" {
		cfg.Server.TProxyAddr = "[::]:10800"
	}
	if len(cfg.FakeIP.CIDRs) == 0 {
		cfg.FakeIP.CIDRs = []string{"fd00::/8"}
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

func (c *FakeIPConfig) ParseCIDRs() ([]net.IP, []*net.IPNet, error) {
	var starts []net.IP
	var nets []*net.IPNet
	for _, cidr := range c.CIDRs {
		ip, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, nil, err
		}
		if ip.To16() == nil {
			return nil, nil, fmt.Errorf("FakeIP 必須使用 IPv6 CIDR: %s", cidr)
		}
		starts = append(starts, ip.To16())
		nets = append(nets, ipnet)
	}
	if len(nets) == 0 {
		return nil, nil, fmt.Errorf("必须至少配置一个有效的 FakeIP CIDR 网段")
	}
	return starts, nets, nil
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
