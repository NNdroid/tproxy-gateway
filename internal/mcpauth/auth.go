// Package mcpauth owns the configurable HTTP credential policy for MCP.
package mcpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

type Config struct {
	Mode         string `yaml:"mode" json:"mode"`
	Token        string `yaml:"token,omitempty" json:"token,omitempty"`
	APIKey       string `yaml:"api_key,omitempty" json:"api_key,omitempty"`
	APIKeyHeader string `yaml:"api_key_header,omitempty" json:"api_key_header,omitempty"`
	Username     string `yaml:"username,omitempty" json:"username,omitempty"`
	Password     string `yaml:"password,omitempty" json:"password,omitempty"`
}

func (c Config) EffectiveMode() string {
	if c.Mode == "" {
		return "inherit"
	}
	return c.Mode
}
func (c Config) KeyHeader() string {
	if c.APIKeyHeader == "" {
		return "X-API-Key"
	}
	return http.CanonicalHeaderKey(c.APIKeyHeader)
}

func Validate(c Config) error {
	switch c.EffectiveMode() {
	case "inherit", "none", "bearer", "api_key", "basic":
	default:
		return errors.New("unsupported MCP authentication mode")
	}
	if c.APIKeyHeader != "" && !validKeyHeader(c.APIKeyHeader) {
		return errors.New("invalid or reserved API key header")
	}
	if len(c.Token) > 4096 || len(c.APIKey) > 4096 || len(c.Password) > 4096 || len(c.Username) > 256 {
		return errors.New("MCP credential exceeds size limit")
	}
	switch c.EffectiveMode() {
	case "bearer":
		if !validHeaderSecret(c.Token) {
			return errors.New("Bearer authentication requires a nonempty token without spaces or control characters")
		}
	case "api_key":
		if !validHeaderSecret(c.APIKey) {
			return errors.New("API key authentication requires a nonempty key without spaces or control characters")
		}
	case "basic":
		if c.Username == "" || c.Password == "" || strings.Contains(c.Username, ":") || hasControl(c.Username) || hasControl(c.Password) {
			return errors.New("Basic authentication requires a username without a colon and a nonempty password")
		}
	}
	return nil
}

func hasControl(value string) bool {
	for _, char := range value {
		if char < 32 || char == 127 {
			return true
		}
	}
	return false
}

func validHeaderSecret(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char <= 32 || char >= 127 {
			return false
		}
	}
	return true
}

func validKeyHeader(header string) bool {
	if len(header) > 64 || header == "" {
		return false
	}
	for _, char := range header {
		if !(char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-') {
			return false
		}
	}
	switch strings.ToLower(header) {
	case "authorization", "host", "origin", "referer", "cookie", "content-type", "content-length", "content-encoding", "connection", "upgrade", "transfer-encoding", "accept", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest":
		return false
	}
	return !strings.HasPrefix(strings.ToLower(header), "mcp-")
}

func equal(a, b string) bool {
	left, right := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1
}

func Authenticate(r *http.Request, c Config, uiSecret string) bool {
	if Validate(c) != nil {
		return false
	} // Invalid settings must never become anonymous.
	switch c.EffectiveMode() {
	case "none":
		return true
	case "inherit":
		if uiSecret == "" {
			return true
		}
		return bearer(r, uiSecret) || equal(r.Header.Get("X-UI-Token"), uiSecret)
	case "bearer":
		return bearer(r, c.Token)
	case "api_key":
		return equal(r.Header.Get(c.KeyHeader()), c.APIKey)
	case "basic":
		username, password, ok := r.BasicAuth()
		userOK, passwordOK := equal(username, c.Username), equal(password, c.Password)
		return ok && userOK && passwordOK
	}
	return false
}

func bearer(r *http.Request, secret string) bool {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	return ok && strings.EqualFold(scheme, "Bearer") && equal(token, secret)
}

func (c Config) Challenge() string {
	if Validate(c) != nil {
		return `Bearer realm="tproxy-gateway-mcp"`
	}
	if c.EffectiveMode() == "basic" {
		return `Basic realm="tproxy-gateway-mcp", charset="UTF-8"`
	}
	if c.EffectiveMode() == "api_key" {
		return `ApiKey realm="tproxy-gateway-mcp", header="` + c.KeyHeader() + `"`
	}
	return `Bearer realm="tproxy-gateway-mcp"`
}

// PublicSettings never contains credential values, only whether they exist.
func (c Config) PublicSettings() map[string]any {
	return map[string]any{"mode": c.EffectiveMode(), "api_key_header": c.KeyHeader(), "username": c.Username,
		"token_configured": c.Token != "", "api_key_configured": c.APIKey != "", "password_configured": c.Password != ""}
}

type Update struct {
	Mode         string  `json:"mode"`
	Token        *string `json:"token,omitempty"`
	APIKey       *string `json:"api_key,omitempty"`
	APIKeyHeader *string `json:"api_key_header,omitempty"`
	Username     *string `json:"username,omitempty"`
	Password     *string `json:"password,omitempty"`
}

func (u Update) Apply(current Config) (Config, error) {
	if u.Mode == "" {
		return Config{}, errors.New("authentication mode is required")
	}
	current.Mode = u.Mode
	if u.APIKeyHeader != nil {
		current.APIKeyHeader = *u.APIKeyHeader
	}
	if u.Username != nil {
		current.Username = *u.Username
	}
	if u.Token != nil && *u.Token != "" {
		current.Token = *u.Token
	}
	if u.APIKey != nil && *u.APIKey != "" {
		current.APIKey = *u.APIKey
	}
	if u.Password != nil && *u.Password != "" {
		current.Password = *u.Password
	}
	return current, Validate(current)
}
