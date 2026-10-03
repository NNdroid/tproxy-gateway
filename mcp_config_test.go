package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const validMCPYAML = `ui:
  enabled: true
  mode: both
  addr: ':9090'
  secret: test-only
metrics:
  enabled: true
  addr: ':9090'
server:
  dns_addr: ':5353'
  tproxy_addr: '[::]:10800'
fake_ip:
  cidrs: ['198.18.0.0/15']
  ttl: 2h
routing:
  default_dns: 'udp://192.0.2.1:53'
  default_upstream: DIRECT
mcp:
  enabled: true
  allow_writes: false
  allowed_origins: []
`

func TestMCPExampleConfiguration(t *testing.T) {
	content, err := os.ReadFile("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	previous := cfg
	t.Cleanup(func() { cfg = previous })
	cfg = new(Config)
	if err := yaml.Unmarshal(content, cfg); err != nil {
		t.Fatal(err)
	}
	result, err := validateMCPConfig(context.Background(), string(content))
	if err != nil || len(result["restart_required"].([]string)) != 0 {
		t.Fatalf("example configuration: %v %v", result, err)
	}
	if !cfg.MCP.Enabled || cfg.MCP.AllowWrites {
		t.Fatal("example must enable read-only MCP")
	}
}

func TestMCPConfigValidationAndRestartBoundary(t *testing.T) {
	previous := cfg
	t.Cleanup(func() { cfg = previous })
	cfg = new(Config)
	if err := yaml.Unmarshal([]byte(validMCPYAML), cfg); err != nil {
		t.Fatal(err)
	}
	result, err := validateMCPConfig(context.Background(), validMCPYAML)
	if err != nil || len(result["restart_required"].([]string)) != 0 || result["ipv4_prefixes"] != 1 || result["ipv6_prefixes"] != 0 {
		t.Fatalf("valid configuration: %v %v", result, err)
	}
	// Write policy can change on the current listener, but listeners/pools/origins cannot.
	result, err = validateMCPConfig(context.Background(), strings.Replace(validMCPYAML, "allow_writes: false", "allow_writes: true", 1))
	if err != nil || len(result["restart_required"].([]string)) != 0 {
		t.Fatalf("write policy: %v %v", result, err)
	}
	for _, pair := range [][2]string{
		{"':5353'", "':5354'"}, {"':9090'", "':9091'"}, {"ttl: 2h", "ttl: 3h"},
		{"default_upstream: DIRECT", "default_upstream: DIRECT\n  fwmark: 88"},
		{"allowed_origins: []", "allowed_origins: ['https://client.example']"},
	} {
		result, err = validateMCPConfig(context.Background(), strings.Replace(validMCPYAML, pair[0], pair[1], 1))
		if err != nil || len(result["restart_required"].([]string)) == 0 {
			t.Fatalf("missing restart boundary for %s: %v %v", pair[0], result, err)
		}
	}
}

func TestMCPConfigRejectsMalformedAndUnknownFields(t *testing.T) {
	for _, content := range []string{
		"", "fake_ip: [", validMCPYAML + "\n---\n{}", validMCPYAML + "\nunknown_field: true",
		strings.Replace(validMCPYAML, "198.18.0.0/15", "invalid-prefix", 1),
		strings.Replace(validMCPYAML, "ttl: 2h", "ttl: -1h", 1),
		strings.Replace(validMCPYAML, "mode: both", "mode: invalid", 1),
		strings.Replace(validMCPYAML, "udp://192.0.2.1:53", "invalid://192.0.2.1:53", 1),
		strings.Replace(validMCPYAML, "udp://192.0.2.1:53", "udp://", 1),
		strings.Replace(validMCPYAML, "allowed_origins: []", "allowed_origins: ['https://client.example/path']", 1),
	} {
		if _, err := validateMCPConfig(context.Background(), content); err == nil {
			t.Fatalf("invalid configuration accepted: %s", content)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := validateMCPConfig(ctx, validMCPYAML); err == nil {
		t.Fatal("canceled validation continued")
	}
}

func TestManagementServicesUseOneAddress(t *testing.T) {
	for _, item := range []struct {
		name    string
		metrics MetricsConfig
		ui      UIConfig
		mcp     bool
		address string
		enabled bool
	}{
		{"preserve existing metrics listener", MetricsConfig{true, ":9090"}, UIConfig{Enabled: true, Mode: "both", Addr: ":9191"}, true, ":9090", true},
		{"UI without metrics", MetricsConfig{}, UIConfig{Enabled: true, Mode: "port", Addr: ":9091"}, true, ":9091", true},
		{"metrics address fallback", MetricsConfig{true, ":9092"}, UIConfig{Enabled: true, Mode: "port"}, true, ":9092", true},
		{"MCP only", MetricsConfig{Addr: ":9093"}, UIConfig{}, true, ":9093", true},
		{"default address", MetricsConfig{}, UIConfig{}, true, ":9090", true},
		{"virtual domain only", MetricsConfig{}, UIConfig{Enabled: true, Mode: "domain"}, true, "", false},
		{"virtual domain plus metrics", MetricsConfig{true, ":9094"}, UIConfig{Enabled: true, Mode: "domain", Addr: ":9191"}, true, ":9094", true},
		{"all disabled", MetricsConfig{}, UIConfig{}, false, "", false},
	} {
		t.Run(item.name, func(t *testing.T) {
			address, enabled := managementListenAddress(item.metrics, item.ui, item.mcp)
			if address != item.address || enabled != item.enabled {
				t.Fatalf("got %q/%v, want %q/%v", address, enabled, item.address, item.enabled)
			}
		})
	}
}
