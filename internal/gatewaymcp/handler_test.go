package gatewaymcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tproxy-gateway/internal/mcpauth"
)

const testSecret = "test-only-secret"

type fixture struct {
	server   *httptest.Server
	enabled  atomic.Bool
	writable atomic.Bool
	mu       sync.Mutex
	calls    []string
	options  Options
}

func newFixture(t *testing.T, writable bool, change ...func(*Options)) *fixture {
	t.Helper()
	f := &fixture{}
	f.enabled.Store(true)
	f.writable.Store(writable)
	mux := http.NewServeMux()
	mux.HandleFunc("/ui/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "shared dashboard") })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testSecret && r.Header.Get("X-UI-Token") != testSecret {
			http.Error(w, "backend auth failed", 401)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		result := map[string]any{"status": "success"}
		switch r.URL.Path {
		case "/api/state":
			result["uptime"] = "1h"
			result["active_tcp_conns"] = 3
		case "/api/analytics":
			top, trend := make([]any, 20), make([]any, 48)
			for i := range top {
				top[i] = map[string]any{"domain": fmt.Sprintf("host%d.example", i), "count": 20 - i}
			}
			for i := range trend {
				trend[i] = map[string]any{"timestamp": fmt.Sprint(i), "dns_count": i}
			}
			result["top_domains"], result["hourly_trend"] = top, trend
		case "/api/logs":
			result["logs"] = []any{map[string]any{"message": "[DNS] cached", "level": "INFO"}, map[string]any{"message": "[TCP] timeout", "level": "ERROR"}, map[string]any{"message": "[DNS] resolved", "level": "INFO"}}
		case "/api/connections":
			result["connections"] = []any{map[string]any{"id": "conn-1", "upstream": "socks5://alice:connection-password@127.0.0.1:1080"}, map[string]any{"id": "conn-2"}}
		case "/api/config":
			if r.Method == "GET" {
				result["content"] = "ui:\n  secret: hidden-password\nproxy_groups:\n  - name: test\n    proxies:\n      - socks5://alice:private-pass@127.0.0.1:1080\n      - bob:other-pass@127.0.0.1:1081\nnested:\n  access_token: token-value\nsubscriptions:\n  - url: https://example.org/private-subscription-path\nrouting:\n  default_dns: doh://example.org/dns-query?token=dns-private-token\n"
			} else {
				result["message"] = "config applied"
			}
		case "/metrics":
			io.WriteString(w, "tproxy_dns_queries_total 7\n")
			return
		case "/api/connections/kill":
			var input closeInput
			json.NewDecoder(r.Body).Decode(&input)
			if input.ID == "missing" {
				result["status"], result["message"] = "error", "Connection not found"
			}
		case "/api/fakeip/lookup":
			result["domain"] = r.URL.Query().Get("domain")
			result["fake_ips"] = []string{"198.18.0.1"}
		case "/api/proxy/ping":
			result["nodes"] = map[string]any{"socks5://alice:ping-password@127.0.0.1:1080": map[string]any{"ms": 2}}
		case "/api/control", "/api/logs/level":
		default:
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(result)
	})
	f.options = Options{
		Backend: mux, Secret: func() string { return testSecret }, Enabled: f.enabled.Load, AllowWrites: f.writable.Load,
		ValidateConfig: func(ctx context.Context, input string) (map[string]any, error) {
			if input == "bad yaml" {
				return nil, fmt.Errorf("invalid YAML")
			}
			restart := []string{}
			if input == "restart-needed" {
				restart = []string{"server listeners"}
			}
			return map[string]any{"valid": true, "restart_required": restart}, nil
		},
		CheckDomain: func(ctx context.Context, domain, ip string) (map[string]any, error) {
			return map[string]any{"domain": domain, "ip": ip, "blocked": false}, nil
		},
		LookupDNS: func(ctx context.Context, domain string) (map[string]any, error) {
			return map[string]any{"domain": domain, "ips": []string{"192.0.2.1"}}, nil
		},
		ReverseFakeIP: func(ctx context.Context, ip string) (map[string]any, error) {
			return map[string]any{"ip": ip, "domain": "example.org"}, nil
		},
	}
	for _, mutate := range change {
		mutate(&f.options)
	}
	handler, err := NewHandler(f.options)
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/mcp", handler)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

type authTransport struct{ base http.RoundTripper }

func (a authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+testSecret)
	return a.base.RoundTrip(r)
}

func connect(t *testing.T, f *fixture, versions ...string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	client := mcp.NewClient(&mcp.Implementation{Name: "gateway-integration-test", Version: "1"}, nil)
	var options mcp.ClientSessionOptions
	if len(versions) > 0 {
		options.ProtocolVersion = versions[0]
	}
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: f.server.URL + "/mcp", HTTPClient: &http.Client{Transport: authTransport{http.DefaultTransport}}, DisableStandaloneSSE: true}, &options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func invoke(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func data(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()
	if result.IsError {
		t.Fatalf("tool error: %+v", result.Content)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if len(result.Content) == 0 {
		t.Fatal("legacy text content missing")
	}
	return output
}

func TestOfficialClientAndSharedPort(t *testing.T) {
	for _, version := range []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"} {
		t.Run(version, func(t *testing.T) {
			const buildVersion = "v1.0.20261003.12-123abcd"
			f := newFixture(t, false, func(options *Options) { options.Version = buildVersion })
			session := connect(t, f, version)
			if got := session.InitializeResult().ServerInfo.Version; got != buildVersion {
				t.Fatalf("MCP server version = %q, want %q", got, buildVersion)
			}
			tools, err := session.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(tools.Tools) != len(ToolNames) {
				t.Fatalf("got %d tools", len(tools.Tools))
			}
			for _, tool := range tools.Tools {
				if tool.InputSchema == nil || tool.OutputSchema == nil || tool.Annotations == nil {
					t.Fatalf("incomplete schema/annotations: %s", tool.Name)
				}
			}
			if got := data(t, invoke(t, session, "gateway_state", map[string]any{}))["uptime"]; got != "1h" {
				t.Fatal(got)
			}
			res, err := http.Get(f.server.URL + "/ui/")
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, _ := io.ReadAll(res.Body)
			if string(body) != "shared dashboard" {
				t.Fatal("MCP replaced WebUI route")
			}
			request, _ := http.NewRequest("GET", f.server.URL+"/api/state", nil)
			request.Header.Set("Authorization", "Bearer "+testSecret)
			res, err = http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatal("REST and MCP do not share the listener")
			}
		})
	}
}

func TestQueriesResourcesAndPrompts(t *testing.T) {
	f := newFixture(t, false)
	session := connect(t, f)
	analytics := data(t, invoke(t, session, "gateway_analytics", map[string]any{"top_n": 2, "hours": 3}))
	if len(analytics["top_domains"].([]any)) != 2 || len(analytics["hourly_trend"].([]any)) != 3 {
		t.Fatal("analytics limits not applied")
	}
	logs := data(t, invoke(t, session, "gateway_logs", map[string]any{"keyword": "dns", "level": "INFO", "limit": 1}))
	if got := logs["logs"].([]any); len(got) != 1 || got[0].(map[string]any)["message"] != "[DNS] resolved" {
		t.Fatal(got)
	}
	connections := data(t, invoke(t, session, "gateway_connections", map[string]any{"limit": 1}))
	if connections["total"] != float64(2) || len(connections["connections"].([]any)) != 1 {
		t.Fatal(connections)
	}
	for name, args := range map[string]map[string]any{
		"gateway_metrics": {}, "gateway_proxy_ping": {}, "gateway_config_validate": {"content": "valid YAML"},
		"gateway_domain_check": {"domain": "EXAMPLE.org.", "ip": "2001:db8::1"},
		"gateway_dns_lookup":   {"domain": "example.org"}, "gateway_fakeip_reverse": {"ip": "198.18.0.1"},
	} {
		data(t, invoke(t, session, name, args))
	}
	config := data(t, invoke(t, session, "gateway_config_get", map[string]any{}))["content"].(string)
	for _, secret := range []string{"hidden-password", "private-pass", "other-pass", "token-value", "private-subscription-path", "dns-private-token"} {
		if strings.Contains(config, secret) {
			t.Fatalf("secret leaked: %s", secret)
		}
	}
	for _, name := range []string{"gateway_connections", "gateway_proxy_ping"} {
		encoded, _ := json.Marshal(data(t, invoke(t, session, name, map[string]any{})))
		if bytes.Contains(encoded, []byte("connection-password")) || bytes.Contains(encoded, []byte("ping-password")) {
			t.Fatal("proxy credentials leaked")
		}
	}
	resources, err := session.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources.Resources) != 7 {
		t.Fatal("resource catalog incomplete")
	}
	for _, resource := range resources.Resources {
		result, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: resource.URI})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Contents) != 1 || result.Contents[0].URI != resource.URI {
			t.Fatal("bad resource contents")
		}
		if strings.Contains(result.Contents[0].Text, "hidden-password") {
			t.Fatal("resource leaked configuration password")
		}
	}
	prompts, err := session.ListPrompts(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts.Prompts) != 2 {
		t.Fatal("prompt catalog incomplete")
	}
	for _, prompt := range prompts.Prompts {
		result, err := session.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: prompt.Name, Arguments: map[string]string{"symptom": "DNS timeout"}})
		if err != nil || len(result.Messages) != 1 {
			t.Fatalf("prompt: %v", err)
		}
	}
}

func TestWritePolicyAndValidationBeforeEffects(t *testing.T) {
	f := newFixture(t, false)
	session := connect(t, f)
	writes := map[string]map[string]any{
		"gateway_config_apply": {"content": "valid YAML"}, "gateway_fakeip_allocate": {"domain": "example.org"},
		"gateway_connection_close": {"id": "conn-1"}, "gateway_log_level": {"level": "DEBUG"}, "gateway_control": {"action": "clear_dns_cache"},
	}
	for name, args := range writes {
		if !invoke(t, session, name, args).IsError {
			t.Fatal("write was allowed:", name)
		}
	}
	f.mu.Lock()
	calls := len(f.calls)
	f.mu.Unlock()
	if calls != 0 {
		t.Fatal("disabled writes reached backend")
	}
	f.writable.Store(true)
	for name, args := range writes {
		data(t, invoke(t, session, name, args))
	}
	invalid := []struct {
		name string
		args map[string]any
	}{
		{"gateway_config_apply", map[string]any{"content": "bad yaml"}},
		{"gateway_config_apply", map[string]any{"content": "restart-needed"}},
		{"gateway_config_apply", map[string]any{"content": "ui: {secret: '[REDACTED]'}"}},
		{"gateway_fakeip_allocate", map[string]any{"domain": "example.org?token=bad"}},
		{"gateway_connection_close", map[string]any{"id": ""}},
		{"gateway_log_level", map[string]any{"level": "TRACE"}},
		{"gateway_control", map[string]any{"action": "shell_command"}},
		{"gateway_analytics", map[string]any{"hours": 721}},
		{"gateway_domain_check", map[string]any{"domain": "http://example.org"}},
		{"gateway_fakeip_reverse", map[string]any{"ip": "not-an-ip"}},
	}
	f.mu.Lock()
	calls = len(f.calls)
	f.mu.Unlock()
	for _, item := range invalid {
		if !invoke(t, session, item.name, item.args).IsError {
			t.Fatal("invalid input accepted:", item.name)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != calls {
		t.Fatal("invalid input had domain effects")
	}
}

func rawRequest(t *testing.T, f *fixture, method, body string, headers map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, f.server.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, data
}

func TestHTTPAuthenticationOriginAndBodyLimits(t *testing.T) {
	f := newFixture(t, true)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	for _, headers := range []map[string]string{nil, {"Authorization": "Bearer wrong"}, {"Authorization": testSecret}} {
		if code, _ := rawRequest(t, f, "POST", body, headers); code != 401 {
			t.Fatalf("auth code: %d", code)
		}
	}
	for _, headers := range []map[string]string{{"Authorization": "Bearer " + testSecret}, {"X-UI-Token": testSecret}} {
		if code, _ := rawRequest(t, f, "POST", body, headers); code != 200 {
			t.Fatalf("valid auth code: %d", code)
		}
	}
	if code, _ := rawRequest(t, f, "POST", body, map[string]string{"Authorization": "Bearer " + testSecret, "Origin": "https://evil.example"}); code != 403 {
		t.Fatalf("cross-origin code: %d", code)
	}
	if code, _ := rawRequest(t, f, "POST", strings.Repeat(" ", MaxRequestBytes+1), map[string]string{"Authorization": "Bearer " + testSecret}); code != 413 {
		t.Fatalf("body limit code: %d", code)
	}
	for _, method := range []string{"GET", "DELETE", "PUT"} {
		if code, _ := rawRequest(t, f, method, "", map[string]string{"Authorization": "Bearer " + testSecret}); code != 405 {
			t.Fatalf("%s code: %d", method, code)
		}
	}
	f.enabled.Store(false)
	if code, _ := rawRequest(t, f, "POST", body, nil); code != 404 {
		t.Fatalf("disabled code: %d", code)
	}
}

func TestLegacyHandshakeNotificationsAndErrors(t *testing.T) {
	f := newFixture(t, false)
	auth := map[string]string{"Authorization": "Bearer " + testSecret}
	for _, version := range []string{"2025-11-25", "2025-06-18", "2025-03-26"} {
		t.Run(version, func(t *testing.T) {
			headers := map[string]string{"Authorization": "Bearer " + testSecret, "Mcp-Protocol-Version": version}
			request := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"raw-test","version":"1"}}}`, version)
			code, body := rawRequest(t, f, "POST", request, headers)
			if code != 200 || !bytes.Contains(body, []byte(`"protocolVersion":"`+version+`"`)) {
				t.Fatalf("handshake %d %s", code, body)
			}
			code, body = rawRequest(t, f, "POST", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"gateway_state","arguments":{}}}`, headers)
			if code != 200 || !bytes.Contains(body, []byte(`"uptime":"1h"`)) {
				t.Fatalf("legacy call %d %s", code, body)
			}
		})
	}
	code, body := rawRequest(t, f, "POST", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"raw-test","version":"1"}}}`, auth)
	if code != 200 || !bytes.Contains(body, []byte(`"protocolVersion":"2025-11-25"`)) {
		t.Fatalf("handshake %d %s", code, body)
	}
	if code, _ = rawRequest(t, f, "POST", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, auth); code != 202 {
		t.Fatalf("notification code: %d", code)
	}
	code, body = rawRequest(t, f, "POST", `{"jsonrpc":"2.0","id":2,"method":"unknown/method"}`, auth)
	if code != 400 || !bytes.Contains(body, []byte(`unsupported`)) {
		t.Fatalf("unknown method: %d %s", code, body)
	}
	code, body = rawRequest(t, f, "POST", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"gateway_state","arguments":{"unexpected":true}}}`, auth)
	if code != 200 {
		t.Fatal(code)
	}
	// Inferred typed input schemas must reject unknown arguments or return a tool error.
	if !bytes.Contains(body, []byte(`"error"`)) && !bytes.Contains(body, []byte(`"isError":true`)) {
		t.Fatalf("unknown field accepted: %s", body)
	}
}

func TestTrustedBrowserOriginAndHeaderOnlyAuthentication(t *testing.T) {
	f := newFixture(t, false, func(options *Options) { options.AllowedOrigins = []string{"https://client.example"} })
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	if code, _ := rawRequest(t, f, "POST", body, map[string]string{"Authorization": "Bearer " + testSecret, "Origin": "https://client.example"}); code != 200 {
		t.Fatalf("trusted origin: %d", code)
	}
	req, _ := http.NewRequest("OPTIONS", f.server.URL+"/mcp", nil)
	req.Header.Set("Origin", "https://client.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type, Mcp-Protocol-Version")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 204 || res.Header.Get("Access-Control-Allow-Origin") != "https://client.example" || !strings.Contains(res.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatal("missing authenticated-browser preflight")
	}
	req, _ = http.NewRequest("POST", f.server.URL+"/mcp?token="+testSecret, strings.NewReader(body))
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal("query-string credential was accepted")
	}
}

func TestDisconnectedClientCancelsLegacyOperation(t *testing.T) {
	started, finished := make(chan struct{}), make(chan struct{})
	f := newFixture(t, false, func(options *Options) {
		options.LookupDNS = func(ctx context.Context, domain string) (map[string]any, error) {
			close(started)
			<-ctx.Done()
			close(finished)
			return nil, ctx.Err()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", f.server.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"gateway_dns_lookup","arguments":{"domain":"example.org"}}}`))
	req.Header.Set("Authorization", "Bearer "+testSecret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	returned := make(chan error, 1)
	go func() {
		res, err := http.DefaultClient.Do(req)
		if res != nil {
			res.Body.Close()
		}
		returned <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not start")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("operation survived client disconnect")
	}
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP request did not finish")
	}
}

func TestOversizedBackendResponseAndMissingConnectionAreToolErrors(t *testing.T) {
	f := newFixture(t, true, func(options *Options) {
		backend := options.Backend
		options.Backend = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/metrics" {
				io.WriteString(w, strings.Repeat("x", maxResponseBytes+1))
				return
			}
			backend.ServeHTTP(w, r)
		})
	})
	session := connect(t, f)
	if !invoke(t, session, "gateway_metrics", map[string]any{}).IsError {
		t.Fatal("oversized backend response accepted")
	}
	if !invoke(t, session, "gateway_connection_close", map[string]any{"id": "missing"}).IsError {
		t.Fatal("backend operation error was reported as success")
	}
}

func TestIndependentAuthenticationModesCanInvokeManagementTools(t *testing.T) {
	var current atomic.Pointer[Policy]
	current.Store(&Policy{Enabled: true, UISecret: testSecret})
	f := newFixture(t, false, func(options *Options) { options.Policy = func() Policy { return *current.Load() } })
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"gateway_state","arguments":{}}}`
	for _, mode := range []string{"inherit", "bearer", "api_key", "basic", "none"} {
		t.Run(mode, func(t *testing.T) {
			policy := Policy{Enabled: true, UISecret: testSecret, Auth: mcpauth.Config{Mode: mode, Token: "independent-token", APIKey: "independent-key", APIKeyHeader: "X-Gateway-Key", Username: "client-user", Password: "client-password"}}
			current.Store(&policy)
			headers := map[string]string{}
			switch mode {
			case "inherit":
				headers["X-UI-Token"] = testSecret
			case "bearer":
				headers["Authorization"] = "Bearer independent-token"
			case "api_key":
				headers["X-Gateway-Key"] = "independent-key"
			case "basic":
				headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte("client-user:client-password"))
			}
			code, response := rawRequest(t, f, "POST", body, headers)
			if code != 200 || !bytes.Contains(response, []byte(`"uptime":"1h"`)) {
				t.Fatalf("MCP credential could not reach trusted management backend: %d %s", code, response)
			}
			if mode != "none" {
				if code, _ := rawRequest(t, f, "POST", body, nil); code != 401 {
					t.Fatal("anonymous MCP request accepted")
				}
			}
			if mode != "inherit" && mode != "none" {
				if code, _ := rawRequest(t, f, "POST", body, map[string]string{"X-UI-Token": testSecret}); code != 401 {
					t.Fatal("explicit mode fell back to UI credential")
				}
			}
		})
	}
	current.Store(&Policy{Enabled: true, UISecret: testSecret, Auth: mcpauth.Config{Mode: "bearer", Token: "new-rotated-token"}})
	if code, _ := rawRequest(t, f, "POST", body, map[string]string{"Authorization": "Bearer independent-token"}); code != 401 {
		t.Fatal("old token accepted after rotation")
	}
	if code, response := rawRequest(t, f, "POST", body, map[string]string{"Authorization": "Bearer new-rotated-token"}); code != 200 || !bytes.Contains(response, []byte(`"uptime":"1h"`)) {
		t.Fatal("new token rejected after rotation")
	}
}
