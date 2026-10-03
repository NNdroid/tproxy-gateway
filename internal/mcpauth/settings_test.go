package mcpauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSettingsHTTPBoundaryAndAdministratorAuth(t *testing.T) {
	config := Config{Mode: "bearer", Token: "saved-only-token"}
	saves := 0
	failSave := false
	handler := NewSettingsHandler(SettingsOptions{
		Authorized: func(r *http.Request) bool { return r.Header.Get("X-UI-Token") == "admin-only" },
		Load:       func() Config { return config },
		Save: func(ctx context.Context, update Update) (Config, error) {
			if failSave {
				return Config{}, errors.New("disk failure")
			}
			candidate, err := update.Apply(config)
			if err != nil {
				return Config{}, err
			}
			config = candidate
			saves++
			return config, nil
		},
	})
	request := func(method, body, media string, admin bool, extra map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://gateway.test/api/mcp/settings", strings.NewReader(body))
		if admin {
			req.Header.Set("X-UI-Token", "admin-only")
		}
		if media != "" {
			req.Header.Set("Content-Type", media)
		}
		for key, value := range extra {
			req.Header.Set(key, value)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	if got := request("GET", "", "", false, map[string]string{"Authorization": "Bearer saved-only-token"}); got.Code != 401 {
		t.Fatal("MCP token could access administrator settings")
	}
	response := request("GET", "", "", true, nil)
	if response.Code != 200 || strings.Contains(response.Body.String(), config.Token) {
		t.Fatal("GET leaked token")
	}
	for _, body := range []string{"", `null`, `{"mode":"none","mode":"bearer"}`, `{"Mode":"none"}`, `{"mode":"none","unknown":true}`, `{"mode":"none"} {}`, `{"mode":"basic"}`, `{"mode":"api_key","api_key":"test","api_key_header":"Origin"}`} {
		if response := request("PUT", body, "application/json", true, nil); response.Code != 400 {
			t.Fatalf("invalid body code %d: %s", response.Code, body)
		}
	}
	if response := request("PUT", `{"mode":"none"}`, "text/plain", true, nil); response.Code != 415 {
		t.Fatal("media boundary")
	}
	if response := request("PUT", `{"mode":"none"}`, "application/json", true, map[string]string{"Content-Encoding": "gzip"}); response.Code != 415 {
		t.Fatal("encoding boundary")
	}
	if response := request("PUT", strings.Repeat(" ", MaxSettingsBytes+1), "application/json", true, nil); response.Code != 413 {
		t.Fatal("body size boundary")
	}
	if response := request("PUT", `{"mode":"none"}`, "application/json", true, map[string]string{"Origin": "https://evil.test"}); response.Code != 403 {
		t.Fatal("cross-origin settings write")
	}
	if response := request("POST", `{"mode":"none"}`, "application/json", true, nil); response.Code != 405 {
		t.Fatal("method boundary")
	}
	if saves != 0 || config.Mode != "bearer" {
		t.Fatal("invalid requests changed configuration")
	}
	response = request("PUT", `{"mode":"basic","username":"user","password":"private-password"}`, "application/json; charset=UTF-8", true, nil)
	if response.Code != 200 || config.Mode != "basic" || saves != 1 || strings.Contains(response.Body.String(), config.Password) {
		t.Fatal("Basic save failed or leaked password")
	}
	failSave = true
	response = request("PUT", `{"mode":"none"}`, "application/json", true, nil)
	if response.Code != 500 || config.Mode != "basic" {
		t.Fatal("failed save changed configuration")
	}
}
