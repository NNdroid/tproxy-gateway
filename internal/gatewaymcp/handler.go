// Package gatewaymcp exposes the gateway's management API over MCP on a shared mux.
package gatewaymcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
	"tproxy-gateway/internal/mcpauth"
)

const MaxRequestBytes = 1 << 20
const maxResponseBytes = 4 << 20

type Options struct {
	Policy         func() Policy
	Backend        http.Handler
	Secret         func() string
	Enabled        func() bool
	AllowWrites    func() bool
	AllowedOrigins []string
	ValidateConfig func(context.Context, string) (map[string]any, error)
	CheckDomain    func(context.Context, string, string) (map[string]any, error)
	LookupDNS      func(context.Context, string) (map[string]any, error)
	ReverseFakeIP  func(context.Context, string) (map[string]any, error)
}

// Each admitted request captures one complete authentication/authorization policy.
type Policy struct {
	Auth        mcpauth.Config
	UISecret    string
	Enabled     bool
	AllowWrites bool
}

func requestPolicy(options Options) Policy {
	if options.Policy != nil {
		return options.Policy()
	}
	policy := Policy{Enabled: true}
	if options.Secret != nil {
		policy.UISecret = options.Secret()
	}
	if options.Enabled != nil {
		policy.Enabled = options.Enabled()
	}
	if options.AllowWrites != nil {
		policy.AllowWrites = options.AllowWrites()
	}
	return policy
}

type requestContext struct {
	headers     http.Header
	allowWrites bool
	ctx         context.Context
}
type requestContextKey struct{}

// NewHandler does not create a listener. Mount it at /mcp on the existing mux.
func NewHandler(options Options) (http.Handler, error) {
	if options.Backend == nil {
		return nil, errors.New("MCP requires a management backend")
	}
	protection := http.NewCrossOriginProtection()
	trustedOrigins := make(map[string]bool)
	for _, origin := range options.AllowedOrigins {
		if err := protection.AddTrustedOrigin(origin); err != nil {
			return nil, fmt.Errorf("invalid MCP allowed origin: %w", err)
		}
		trustedOrigins[origin] = true
	}
	server := newServer(options)
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, MaxRequestBodyBytes: MaxRequestBytes,
		PropagateRequestCancellation: true,
	})
	protected := protection.Handler(transport)
	admission := make(chan struct{}, 32)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		policy := requestPolicy(options)
		w.Header().Set("Cache-Control", "no-store")
		if !policy.Enabled {
			http.NotFound(w, r)
			return
		}
		if origin := r.Header.Get("Origin"); trustedOrigins[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Expose-Headers", "Mcp-Protocol-Version")
			if r.Method == http.MethodOptions {
				if r.Header.Get("Access-Control-Request-Method") != http.MethodPost {
					http.Error(w, "only MCP POST is supported", http.StatusMethodNotAllowed)
					return
				}
				w.Header().Set("Access-Control-Allow-Methods", "POST")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-UI-Token, Mcp-Protocol-Version, Mcp-Method, Mcp-Name")
				if policy.Auth.EffectiveMode() == "api_key" && mcpauth.Validate(policy.Auth) == nil {
					w.Header().Set("Access-Control-Allow-Headers", w.Header().Get("Access-Control-Allow-Headers")+", "+policy.Auth.KeyHeader())
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		if !mcpauth.Authenticate(r, policy.Auth, policy.UISecret) {
			w.Header().Set("WWW-Authenticate", policy.Auth.Challenge())
			http.Error(w, "MCP authentication required", http.StatusUnauthorized)
			return
		}
		select {
		case admission <- struct{}{}:
			defer func() { <-admission }()
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "MCP server is busy", http.StatusTooManyRequests)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Now().Add(30 * time.Second))
		defer rc.SetReadDeadline(time.Time{})
		meta := requestContext{headers: make(http.Header), ctx: ctx}
		// This trusted in-process invocation follows successful MCP authentication.
		// MCP credentials are never reused as WebUI administrator credentials.
		meta.headers.Set("X-UI-Token", policy.UISecret)
		meta.allowWrites = policy.AllowWrites
		ctx = context.WithValue(ctx, requestContextKey{}, meta)
		protected.ServeHTTP(w, r.WithContext(ctx))
	}), nil
}

// Legacy SDK sessions may detach their request context. Operations still use the
// original HTTP cancellation and deadline, while keeping authenticated metadata.
func operationContext(ctx context.Context) context.Context {
	if meta, ok := ctx.Value(requestContextKey{}).(requestContext); ok {
		return context.WithValue(meta.ctx, requestContextKey{}, meta)
	}
	return ctx
}

type capturedResponse struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	tooLarge bool
}

func (w *capturedResponse) Header() http.Header { return w.header }
func (w *capturedResponse) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}
func (w *capturedResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.body.Len()+len(data) > maxResponseBytes {
		w.tooLarge = true
		return 0, errors.New("management response exceeds MCP limit")
	}
	return w.body.Write(data)
}

func callAPI(ctx context.Context, options Options, method, path string, body []byte) (map[string]any, error) {
	meta, ok := ctx.Value(requestContextKey{}).(requestContext)
	if !ok {
		return nil, errors.New("missing authenticated MCP request context")
	}
	if err := meta.ctx.Err(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(meta.ctx, method, path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = meta.headers.Clone()
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
		if path == "/api/config" {
			req.Header.Set("Content-Type", "text/plain")
		}
	}
	response := &capturedResponse{header: make(http.Header)}
	options.Backend.ServeHTTP(response, req)
	if response.tooLarge {
		return nil, errors.New("management response exceeds the MCP 4 MiB limit")
	}
	if err := meta.ctx.Err(); err != nil {
		return nil, err
	}
	if response.status >= 400 {
		return nil, fmt.Errorf("management API HTTP %d", response.status)
	}
	if path == "/metrics" {
		return map[string]any{"metrics": response.body.String()}, nil
	}
	var result map[string]any
	if err := json.Unmarshal(response.body.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("invalid management response: %w", err)
	}
	if result["status"] == "error" {
		return nil, fmt.Errorf("management operation failed: %v", result["message"])
	}
	return result, nil
}

func checkWrite(ctx context.Context) error {
	meta, ok := ctx.Value(requestContextKey{}).(requestContext)
	if !ok || !meta.allowWrites {
		return errors.New("MCP write operations are disabled; enable mcp.allow_writes in gateway configuration")
	}
	if err := meta.ctx.Err(); err != nil {
		return err
	}
	return nil
}

func redactedConfig(ctx context.Context, options Options) (map[string]any, error) {
	result, err := callAPI(ctx, options, "GET", "/api/config", nil)
	if err != nil {
		return nil, err
	}
	content, ok := result["content"].(string)
	if !ok {
		return nil, errors.New("configuration response contains no YAML content")
	}
	var config map[string]any
	if err := yaml.Unmarshal([]byte(content), &config); err != nil {
		return nil, errors.New("configuration is not valid YAML")
	}
	encoded, err := yaml.Marshal(redact(config))
	if err != nil {
		return nil, err
	}
	result["content"] = string(encoded)
	result["redacted"] = true
	return result, nil
}

func redact(value any) any {
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			name := strings.ToLower(key)
			if sensitiveKey(name) || name == "url" {
				value[key] = "[REDACTED]"
			} else {
				value[key] = redact(item)
			}
		}
		return value
	case []any:
		for i, item := range value {
			value[i] = redact(item)
		}
		return value
	case string:
		if parsed, err := url.Parse(value); err == nil && parsed.IsAbs() && parsed.Host != "" {
			changed := false
			if parsed.User != nil {
				parsed.User = url.User("REDACTED")
				changed = true
			}
			query := parsed.Query()
			for key := range query {
				if sensitiveKey(strings.ToLower(key)) {
					query.Set(key, "[REDACTED]")
					changed = true
				}
			}
			if changed {
				parsed.RawQuery = query.Encode()
				return parsed.String()
			}
		}
		if strings.Contains(value, "@") {
			if parsed, err := url.Parse(value); err == nil && parsed.User != nil {
				parsed.User = url.User("REDACTED")
				return parsed.String()
			}
			return "REDACTED@" + value[strings.LastIndexByte(value, '@')+1:]
		}
		return value
	default:
		return value
	}
}

func sensitiveKey(name string) bool {
	return name == "secret" || name == "password" || name == "token" || name == "private_key" || name == "api_key" || name == "apikey" || name == "authorization" || strings.HasSuffix(name, "_secret") || strings.HasSuffix(name, "_token") || strings.HasSuffix(name, "_password")
}

func canonicalDomain(domain string) (string, error) {
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if domain == "" || len(domain) > 253 || strings.ContainsAny(domain, ":/\\ \t\r\n?#@") {
		return "", errors.New("domain must be a plain DNS hostname, without a URL, port or path")
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", errors.New("invalid DNS label")
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
				return "", errors.New("use ASCII or punycode DNS labels")
			}
		}
	}
	return domain, nil
}

func encodeBody(value any) []byte { data, _ := json.Marshal(value); return data }
