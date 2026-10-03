package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"tproxy-gateway/internal/mcpauth"
)

func authPointer(value string) *string { return &value }

func authSettingsFixture(t *testing.T) (string, *Config) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "# retained gateway comment\n" + validMCPYAML + "# retained extension\nextension: {custom: preserved}\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	configuration := new(Config)
	if err := yaml.Unmarshal([]byte(content), configuration); err != nil {
		t.Fatal(err)
	}
	previousPath, previousPolicy := ActiveConfigPath, liveMCPPolicy.Load()
	t.Cleanup(func() { ActiveConfigPath = previousPath; liveMCPPolicy.Store(previousPolicy) })
	ActiveConfigPath = path
	publishMCPPolicy(configuration)
	return path, configuration
}

func TestMCPAuthSavePersistsWithoutGatewayReload(t *testing.T) {
	path, original := authSettingsFixture(t)
	result, err := saveMCPAuthentication(context.Background(), path, mcpauth.Update{Mode: "basic", Username: authPointer("mcp-user"), Password: authPointer("mcp-only-password")})
	if err != nil || result.Mode != "basic" {
		t.Fatalf("save: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted Config
	if err := yaml.Unmarshal(body, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.MCP.Auth.Password != "mcp-only-password" || persisted.UI != original.UI || persisted.Server != original.Server || persisted.Routing != original.Routing || !strings.Contains(string(body), "retained gateway comment") || !strings.Contains(string(body), "custom: preserved") {
		t.Fatal("save changed unrelated config or failed to persist credentials")
	}
	current := currentMCPPolicy()
	if current.Config.Auth.Password != "mcp-only-password" || current.UISecret != original.UI.Secret || current.Config.Enabled != original.MCP.Enabled || current.Config.AllowWrites != original.MCP.AllowWrites {
		t.Fatal("runtime auth policy did not update independently")
	}
	// A full reload started before the auth save must not restore its old auth.
	publishMCPPolicy(original)
	if currentMCPPolicy().Config.Auth.Password != "mcp-only-password" {
		t.Fatal("stale full reload replaced new auth")
	}
	if _, err := saveMCPAuthentication(context.Background(), path, mcpauth.Update{Mode: "basic", Password: authPointer("")}); err != nil {
		t.Fatal("blank credentials should preserve password")
	}
	reloaded, err := LoadConfig(path)
	if err != nil || reloaded.MCP.Auth.Password != "mcp-only-password" {
		t.Fatal("restart did not retain auth settings")
	}
}

func TestMCPAuthFailedSaveAndValidationLeavePolicyUnchanged(t *testing.T) {
	path, _ := authSettingsFixture(t)
	initial, _ := os.ReadFile(path)
	before := currentMCPPolicy().Config.Auth
	for _, update := range []mcpauth.Update{{Mode: "basic"}, {Mode: "invalid"}, {Mode: "bearer", Token: authPointer("invalid token")}} {
		if _, err := saveMCPAuthentication(context.Background(), path, update); err == nil {
			t.Fatal("invalid credentials saved")
		}
		body, _ := os.ReadFile(path)
		if string(body) != string(initial) || currentMCPPolicy().Config.Auth != before {
			t.Fatal("validation changed file or auth")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := saveMCPAuthentication(ctx, path, mcpauth.Update{Mode: "none"}); err == nil {
		t.Fatal("canceled save succeeded")
	}
	if _, err := saveMCPAuthentication(context.Background(), filepath.Join(t.TempDir(), "missing.yaml"), mcpauth.Update{Mode: "none"}); err == nil {
		t.Fatal("missing file save succeeded")
	}
	if currentMCPPolicy().Config.Auth != before {
		t.Fatal("failed save changed runtime")
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".mcp-auth-*.tmp"))
	if len(files) != 0 {
		t.Fatal("temporary credential file left behind")
	}
}
