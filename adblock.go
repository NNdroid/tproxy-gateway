package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

type blockSnapshot struct{ domains map[string]struct{} }

type AdBlocker struct {
	snapshot  atomic.Pointer[blockSnapshot]
	refreshMu sync.Mutex
}

var globalAdBlocker *AdBlocker

func NewAdBlocker(cfg AdBlockConfig) *AdBlocker {
	ab := &AdBlocker{}
	ab.snapshot.Store(&blockSnapshot{domains: make(map[string]struct{})})
	globalAdBlocker = ab
	if cfg.Enabled {
		go ab.loadAll(cfg)
	}
	return ab
}

// Build a complete immutable snapshot; DNS lookups never wait for I/O or parsing.
func (ab *AdBlocker) loadAll(cfg AdBlockConfig) {
	ab.refreshMu.Lock()
	defer ab.refreshMu.Unlock()
	domains := make(map[string]struct{})
	successful := 0
	for _, path := range cfg.Files {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		err = parseBlocklist(file, domains)
		file.Close()
		if err == nil {
			successful++
		}
	}
	client := &http.Client{Timeout: 10 * time.Second}
	for _, source := range cfg.URLs {
		resp, err := client.Get(source)
		if err != nil {
			zap.S().Warnf("[AdBlock] Failed to download remote blocklist %s: %v", source, err)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			if err := parseBlocklist(resp.Body, domains); err == nil {
				successful++
			}
		}
		resp.Body.Close()
	}
	// Keep the previous snapshot on a failed or incomplete refresh. Empty
	// successful sources (or no configured sources) intentionally remove rules.
	if successful != len(cfg.Files)+len(cfg.URLs) {
		return
	}
	ab.snapshot.Store(&blockSnapshot{domains: domains})
	zap.S().Infof("[AdBlock] Filter refreshed (%d unique rules)", len(domains))
}

func parseBlocklist(reader io.Reader, domains map[string]struct{}) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 && (parts[0] == "0.0.0.0" || parts[0] == "127.0.0.1" || parts[0] == "::") {
			for _, domain := range parts[1:] {
				if strings.HasPrefix(domain, "#") {
					break
				}
				domains[strings.ToLower(strings.TrimSuffix(domain, "."))] = struct{}{}
			}
		} else if len(parts) == 1 {
			domain := strings.TrimPrefix(parts[0], "||")
			domain = strings.TrimSuffix(domain, "^")
			if !strings.ContainsAny(domain, "/:*^|@") {
				domains[strings.ToLower(strings.TrimSuffix(domain, "."))] = struct{}{}
			}
		}
	}
	return scanner.Err()
}

func (ab *AdBlocker) IsBlocked(domain string) bool {
	if ab == nil {
		return false
	}
	snapshot := ab.snapshot.Load()
	if snapshot == nil || len(snapshot.domains) == 0 {
		return false
	}
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	for {
		if _, exists := snapshot.domains[domain]; exists {
			return true
		}
		idx := strings.IndexByte(domain, '.')
		if idx == -1 {
			return false
		}
		domain = domain[idx+1:]
	}
}

func (ab *AdBlocker) StartAutoRefresh(ctx context.Context, cfg AdBlockConfig) {
	if !cfg.Enabled || (len(cfg.URLs) == 0 && len(cfg.Files) == 0) {
		return
	}
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ab.loadAll(cfg)
		}
	}
}
