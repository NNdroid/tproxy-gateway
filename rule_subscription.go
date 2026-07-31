package main

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

type SubscriptionManager struct {
	subs []SubConfig
}

var globalSubManager *SubscriptionManager

func NewSubscriptionManager(subs []SubConfig) *SubscriptionManager {
	sm := &SubscriptionManager{subs: subs}
	globalSubManager = sm
	return sm
}

func (sm *SubscriptionManager) Start(ctx context.Context) {
	if sm == nil || len(sm.subs) == 0 {
		return
	}

	zap.S().Infof("🔄 Rule subscription auto-updater started (%d subscriptions loaded)", len(sm.subs))

	for _, sub := range sm.subs {
		go sm.runSubUpdater(ctx, sub)
	}
}

func (sm *SubscriptionManager) runSubUpdater(ctx context.Context, sub SubConfig) {
	interval, err := time.ParseDuration(sub.Interval)
	if err != nil || interval <= 0 {
		interval = 12 * time.Hour
	}

	sm.fetchAndApply(sub)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sm.fetchAndApply(sub)
		}
	}
}

func (sm *SubscriptionManager) fetchAndApply(sub SubConfig) {
	client := http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(sub.URL)
	if err != nil {
		zap.S().Warnf("[Subscription] Failed to fetch rule subscription [%s] %s: %v", sub.Name, sub.URL, err)
		return
	}
	defer resp.Body.Close()

	var newRules []RuleConfig
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}

		parts := strings.Split(line, ",")
		if len(parts) >= 3 {
			rType := strings.TrimSpace(parts[0])
			payload := strings.TrimSpace(parts[1])
			proxy := strings.TrimSpace(parts[2])
			newRules = append(newRules, RuleConfig{
				Type:    rType,
				Payload: payload,
				Proxy:   proxy,
			})
		}
	}

	if len(newRules) > 0 {
		NewRuleEngine(append(cfg.Rules, newRules...))
		zap.S().Infof("[Subscription] Rule subscription [%s] loaded successfully: %d rules updated", sub.Name, len(newRules))
	}
}
