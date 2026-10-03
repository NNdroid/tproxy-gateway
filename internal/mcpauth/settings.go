package mcpauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

const MaxSettingsBytes = 16 << 10

type SettingsOptions struct {
	Authorized func(*http.Request) bool
	Load       func() Config
	Save       func(context.Context, Update) (Config, error)
}

func NewSettingsHandler(options SettingsOptions) http.Handler {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if options.Authorized == nil || !options.Authorized(r) {
			settingsError(w, "WebUI administrator authentication required", 401)
			return
		}
		if r.Method == "GET" {
			json.NewEncoder(w).Encode(options.Load().PublicSettings())
			return
		}
		if r.Method != "PUT" {
			w.Header().Set("Allow", "GET, PUT")
			settingsError(w, "method not allowed", 405)
			return
		}
		media, parameters, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" || parameters["charset"] != "" && !strings.EqualFold(parameters["charset"], "utf-8") || r.Header.Get("Content-Encoding") != "" && !strings.EqualFold(r.Header.Get("Content-Encoding"), "identity") {
			settingsError(w, "application/json with UTF-8 encoding required", 415)
			return
		}
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Now().Add(30 * time.Second))
		defer rc.SetReadDeadline(time.Time{})
		r.Body = http.MaxBytesReader(w, r.Body, MaxSettingsBytes)
		data, err := io.ReadAll(r.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				settingsError(w, "authentication settings too large", 413)
			} else {
				settingsError(w, "invalid authentication settings body", 400)
			}
			return
		}
		input, err := decodeUpdate(data)
		if err != nil {
			settingsError(w, "invalid authentication settings JSON", 400)
			return
		}
		if _, err := input.Apply(options.Load()); err != nil {
			settingsError(w, err.Error(), 400)
			return
		}
		if r.Context().Err() != nil {
			return
		}
		result, err := options.Save(r.Context(), input)
		if err != nil {
			settingsError(w, "could not save authentication settings; current policy unchanged", 500)
			return
		}
		json.NewEncoder(w).Encode(result.PublicSettings())
	})
	return http.NewCrossOriginProtection().Handler(handler)
}

func settingsError(w http.ResponseWriter, message string, code int) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": message})
}

// The wire object is flat. Enforce exact keys and reject duplicate members before
// decoding so different interpretations cannot select different auth policies.
func decodeUpdate(data []byte) (Update, error) {
	var input Update
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return input, errors.New("expected object")
	}
	allowed := map[string]bool{"mode": true, "token": true, "api_key": true, "api_key_header": true, "username": true, "password": true}
	seen := make(map[string]bool)
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || !allowed[name] || seen[name] {
			return input, errors.New("invalid or duplicate member")
		}
		seen[name] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return input, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return input, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return input, errors.New("trailing JSON")
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return input, err
	}
	return input, nil
}
