package mcpauth

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func pointer(value string) *string { return &value }

func TestAuthenticationModesAndIsolation(t *testing.T) {
	for _, mode := range []string{"inherit", "bearer", "api_key", "basic", "none"} {
		t.Run(mode, func(t *testing.T) {
			config := Config{Mode: mode, Token: "mcp-only-token", APIKey: "mcp-only-key", APIKeyHeader: "X-Gateway-Key", Username: "mcp-user", Password: "mcp-password"}
			req := httptest.NewRequest("POST", "/mcp?token=mcp-only-token", nil)
			if Authenticate(req, config, "ui-only-secret") != (mode == "none") {
				t.Fatal("anonymous/query credential policy")
			}
			switch mode {
			case "inherit":
				req.Header.Set("X-UI-Token", "ui-only-secret")
			case "bearer":
				req.Header.Set("Authorization", "bearer mcp-only-token")
			case "api_key":
				req.Header.Set("X-Gateway-Key", "mcp-only-key")
			case "basic":
				req.SetBasicAuth("mcp-user", "mcp-password")
			}
			if !Authenticate(req, config, "ui-only-secret") {
				t.Fatal("valid credential rejected")
			}
			req = httptest.NewRequest("POST", "/mcp", nil)
			req.Header.Set("Authorization", "Bearer ui-only-secret")
			if Authenticate(req, config, "ui-only-secret") != (mode == "inherit" || mode == "none") {
				t.Fatal("MCP policy accepted unrelated WebUI credential")
			}
		})
	}
	if !Authenticate(httptest.NewRequest("POST", "/mcp", nil), Config{}, "") {
		t.Fatal("legacy anonymous compatibility")
	}
}

func TestInvalidAuthenticationFailsClosed(t *testing.T) {
	for _, config := range []Config{
		{Mode: "unknown"}, {Mode: "bearer"}, {Mode: "bearer", Token: "space token"},
		{Mode: "api_key"}, {Mode: "api_key", APIKey: "test", APIKeyHeader: "Origin"},
		{Mode: "api_key", APIKey: "test", APIKeyHeader: "X-Key\r\nInjected"},
		{Mode: "basic", Username: "name:colon", Password: "test"}, {Mode: "basic", Username: "test"},
		{Mode: "basic", Username: "test", Password: "control\x00password"},
	} {
		if Validate(config) == nil || Authenticate(httptest.NewRequest("POST", "/mcp", nil), config, "") {
			t.Fatal("invalid config did not fail closed")
		}
		if strings.ContainsAny(config.Challenge(), "\r\n") {
			t.Fatal("unsafe auth challenge")
		}
	}
}

func TestSettingsKeepSecretsWithoutReturningThem(t *testing.T) {
	current := Config{Mode: "basic", Username: "test-user", Password: "saved-password", Token: "saved-token", APIKey: "saved-key"}
	updated, err := (Update{Mode: "basic", Password: pointer("")}).Apply(current)
	if err != nil || updated.Password != current.Password || updated.Username != current.Username {
		t.Fatal("blank secret overwrote saved settings")
	}
	updated, err = (Update{Mode: "bearer", Token: pointer("replacement-token")}).Apply(updated)
	if err != nil || updated.Token != "replacement-token" {
		t.Fatal("credential replacement failed")
	}
	encoded, _ := json.Marshal(updated.PublicSettings())
	for _, secret := range []string{"replacement-token", "saved-password", "saved-key"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("credential leaked through public settings")
		}
	}
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer saved-token")
	if Authenticate(req, updated, "") {
		t.Fatal("old token still accepted")
	}
	req.Header.Set("Authorization", "Bearer replacement-token")
	if !Authenticate(req, updated, "") {
		t.Fatal("replacement token rejected")
	}
}
