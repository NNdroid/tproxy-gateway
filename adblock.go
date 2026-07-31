package main

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

type AdBlocker struct {
	blocked map[string]struct{}
	mu      sync.RWMutex
}

var globalAdBlocker *AdBlocker

func NewAdBlocker(cfg AdBlockConfig) *AdBlocker {
	ab := &AdBlocker{
		blocked: make(map[string]struct{}),
	}
	globalAdBlocker = ab

	if !cfg.Enabled {
		return ab
	}

	go ab.loadAll(cfg)
	return ab
}

func (ab *AdBlocker) loadAll(cfg AdBlockConfig) {
	ab.mu.Lock()
	defer ab.mu.Unlock()

	total := 0
	for _, fpath := range cfg.Files {
		count := ab.loadFile(fpath)
		total += count
	}

	for _, u := range cfg.URLs {
		count := ab.loadURL(u)
		total += count
	}

	if total > 0 {
		zap.S().Infof("🛡️ AdBlock filter initialized (%d rules loaded)", total)
	}
}

func (ab *AdBlocker) loadFile(fpath string) int {
	file, err := os.Open(fpath)
	if err != nil {
		return 0
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	count := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) >= 2 && (parts[0] == "0.0.0.0" || parts[0] == "127.0.0.1") {
			domain := strings.TrimSuffix(parts[1], ".")
			ab.blocked[strings.ToLower(domain)] = struct{}{}
			count++
		} else if len(parts) == 1 {
			domain := strings.TrimSuffix(parts[0], ".")
			ab.blocked[strings.ToLower(domain)] = struct{}{}
			count++
		}
	}
	return count
}

func (ab *AdBlocker) loadURL(u string) int {
	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		zap.S().Warnf("[AdBlock] Failed to download remote blocklist %s: %v", u, err)
		return 0
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	count := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) >= 2 && (parts[0] == "0.0.0.0" || parts[0] == "127.0.0.1") {
			domain := strings.TrimSuffix(parts[1], ".")
			ab.blocked[strings.ToLower(domain)] = struct{}{}
			count++
		} else if len(parts) == 1 {
			domain := strings.TrimSuffix(parts[0], ".")
			ab.blocked[strings.ToLower(domain)] = struct{}{}
			count++
		}
	}
	return count
}

func (ab *AdBlocker) IsBlocked(domain string) bool {
	if ab == nil || len(ab.blocked) == 0 {
		return false
	}
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))

	ab.mu.RLock()
	defer ab.mu.RUnlock()

	if _, exists := ab.blocked[domain]; exists {
		return true
	}

	for {
		idx := strings.IndexByte(domain, '.')
		if idx == -1 {
			break
		}
		domain = domain[idx+1:]
		if _, exists := ab.blocked[domain]; exists {
			return true
		}
	}

	return false
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
			zap.S().Infof("[AdBlock] Refreshing AdBlock blocklists...")
			ab.loadAll(cfg)
		}
	}
}
