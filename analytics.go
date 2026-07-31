package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

const MaxHourlyBuckets = 30 * 24 // 30 天 * 24 小时 = 720 个小时桶

type HourlyStat struct {
	Timestamp string `json:"timestamp"` // "YYYY-MM-DD HH:00"
	DNSCount  int64  `json:"dns_count"`
	TCPCount  int64  `json:"tcp_count"`
	UDPCount  int64  `json:"udp_count"`
}

type DomainStat struct {
	Domain string `json:"domain"`
	Count  int64  `json:"count"`
}

type AnalyticsData struct {
	TopDomains map[string]int64       `json:"top_domains"`
	Hourly     map[string]*HourlyStat `json:"hourly"`
}

type AnalyticsStore struct {
	mu         sync.RWMutex
	topDomains map[string]int64
	hourly     map[string]*HourlyStat
	savePath   string
	isDirty    atomic.Bool
}

var GlobalAnalytics *AnalyticsStore

func InitAnalytics(ctx context.Context, savePath string) *AnalyticsStore {
	if savePath == "" {
		savePath = "analytics.json"
	}

	as := &AnalyticsStore{
		topDomains: make(map[string]int64),
		hourly:     make(map[string]*HourlyStat),
		savePath:   savePath,
	}

	as.loadFromFile()
	GlobalAnalytics = as

	go as.startBackgroundTasks(ctx)
	return as
}

func (as *AnalyticsStore) RecordDNSQuery(domain string) {
	if as == nil || domain == "" {
		return
	}
	cleanDomain := strings.ToLower(strings.TrimSuffix(domain, "."))

	nowKey := time.Now().Format("2006-01-02 15:00")

	as.mu.Lock()
	as.topDomains[cleanDomain]++

	stat, exists := as.hourly[nowKey]
	if !exists {
		stat = &HourlyStat{Timestamp: nowKey}
		as.hourly[nowKey] = stat
		as.pruneOldBucketsLocked()
	}
	stat.DNSCount++
	as.mu.Unlock()

	as.isDirty.Store(true)
}

func (as *AnalyticsStore) RecordTCPConn(domain string) {
	if as == nil {
		return
	}
	nowKey := time.Now().Format("2006-01-02 15:00")

	as.mu.Lock()
	stat, exists := as.hourly[nowKey]
	if !exists {
		stat = &HourlyStat{Timestamp: nowKey}
		as.hourly[nowKey] = stat
		as.pruneOldBucketsLocked()
	}
	stat.TCPCount++
	as.mu.Unlock()

	as.isDirty.Store(true)
}

func (as *AnalyticsStore) RecordUDPConn(domain string) {
	if as == nil {
		return
	}
	nowKey := time.Now().Format("2006-01-02 15:00")

	as.mu.Lock()
	stat, exists := as.hourly[nowKey]
	if !exists {
		stat = &HourlyStat{Timestamp: nowKey}
		as.hourly[nowKey] = stat
		as.pruneOldBucketsLocked()
	}
	stat.UDPCount++
	as.mu.Unlock()

	as.isDirty.Store(true)
}

func (as *AnalyticsStore) pruneOldBucketsLocked() {
	if len(as.hourly) <= MaxHourlyBuckets {
		return
	}

	keys := make([]string, 0, len(as.hourly))
	for k := range as.hourly {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	excess := len(keys) - MaxHourlyBuckets
	for i := 0; i < excess; i++ {
		delete(as.hourly, keys[i])
	}
}

func (as *AnalyticsStore) GetSummary(topN int) (top []DomainStat, trend []HourlyStat) {
	if as == nil {
		return nil, nil
	}

	as.mu.RLock()
	defer as.mu.RUnlock()

	// 1. Sort Top Domains
	domainList := make([]DomainStat, 0, len(as.topDomains))
	for d, c := range as.topDomains {
		domainList = append(domainList, DomainStat{Domain: d, Count: c})
	}

	sort.Slice(domainList, func(i, j int) bool {
		return domainList[i].Count > domainList[j].Count
	})

	if len(domainList) > topN {
		domainList = domainList[:topN]
	}
	top = domainList

	// 2. Sort Hourly Trend
	trendList := make([]HourlyStat, 0, len(as.hourly))
	for _, stat := range as.hourly {
		trendList = append(trendList, *stat)
	}

	sort.Slice(trendList, func(i, j int) bool {
		return trendList[i].Timestamp < trendList[j].Timestamp
	})

	return top, trendList
}

func (as *AnalyticsStore) Clear() {
	if as == nil {
		return
	}
	as.mu.Lock()
	as.topDomains = make(map[string]int64)
	as.hourly = make(map[string]*HourlyStat)
	as.mu.Unlock()
	as.isDirty.Store(true)
	as.saveToFileIfDirty()
	zap.S().Infof("[Analytics] Telemetry analytics data cleared via WebUI control")
}

func (as *AnalyticsStore) startBackgroundTasks(ctx context.Context) {
	saveTicker := time.NewTicker(30 * time.Second)
	defer saveTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			as.saveToFileIfDirty()
			return
		case <-saveTicker.C:
			as.saveToFileIfDirty()
		}
	}
}

func (as *AnalyticsStore) saveToFileIfDirty() {
	if !as.isDirty.Load() {
		return
	}

	as.mu.RLock()
	data := AnalyticsData{
		TopDomains: as.topDomains,
		Hourly:     as.hourly,
	}
	encoded, err := json.MarshalIndent(data, "", "  ")
	as.mu.RUnlock()

	if err != nil {
		return
	}

	as.isDirty.Store(false)
	tempFile := as.savePath + ".tmp"
	if err := os.WriteFile(tempFile, encoded, 0644); err == nil {
		os.Rename(tempFile, as.savePath)
	}
}

func (as *AnalyticsStore) loadFromFile() {
	content, err := os.ReadFile(as.savePath)
	if err != nil {
		return
	}

	var data AnalyticsData
	if err := json.Unmarshal(content, &data); err != nil {
		return
	}

	as.mu.Lock()
	if data.TopDomains != nil {
		as.topDomains = data.TopDomains
	}
	if data.Hourly != nil {
		as.hourly = data.Hourly
		as.pruneOldBucketsLocked()
	}
	as.mu.Unlock()
	zap.S().Infof("[Analytics] Loaded historical telemetry data (%d domains, %d hourly buckets)", len(as.topDomains), len(as.hourly))
}

func registerAnalyticsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/analytics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !validateAuthToken(r) {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"status": "unauthorized", "message": "Authentication required"})
			return
		}
		top, trend := GlobalAnalytics.GetSummary(20)
		payload := map[string]interface{}{
			"top_domains":  top,
			"hourly_trend": trend,
		}
		json.NewEncoder(w).Encode(payload)
	})
}
