package main

import (
	"fmt"
	"net"
	"os"

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
}

type FakeIPConfig struct {
	CIDR        string `yaml:"cidr"`
	TTL         string `yaml:"ttl"`
	PersistFile string `yaml:"persist_file"`
}

type RuleConfig struct {
	Proxy   string   `yaml:"proxy"`
	Domains []string `yaml:"domains"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %v", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 YAML 失败: %v", err)
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
	if cfg.FakeIP.CIDR == "" {
		cfg.FakeIP.CIDR = "fd00::/8"
	}
	if cfg.Routing.DefaultUpstream == "" {
		cfg.Routing.DefaultUpstream = "DIRECT"
	}

	return &cfg, nil
}

func (c *FakeIPConfig) ParseCIDR() (net.IP, *net.IPNet, error) {
	ip, ipnet, err := net.ParseCIDR(c.CIDR)
	if err != nil {
		return nil, nil, err
	}
	if ip.To16() == nil {
		return nil, nil, fmt.Errorf("FakeIP 必须使用 IPv6 CIDR: %s", c.CIDR)
	}
	return ip.To16(), ipnet, nil
}