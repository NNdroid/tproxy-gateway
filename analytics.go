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
	saveMu     sync.Mutex
	topDomains map[string]int64
	hourly     map[string]*HourlyStat
	hourID     int64
	hourStat   *HourlyStat
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

	now := time.Now()

	as.mu.Lock()
	as.topDomains[cleanDomain]++

	stat := as.currentBucketLocked(now)
	stat.DNSCount++
	as.mu.Unlock()

	as.isDirty.Store(true)
}

func (as *AnalyticsStore) RecordTCPConn(domain string) {
	if as == nil {
		return
	}
	now := time.Now()

	as.mu.Lock()
	stat := as.currentBucketLocked(now)
	stat.TCPCount++
	as.mu.Unlock()

	as.isDirty.Store(true)
}

func (as *AnalyticsStore) RecordUDPConn(domain string) {
	if as == nil {
		return
	}
	now := time.Now()

	as.mu.Lock()
	stat := as.currentBucketLocked(now)
	stat.UDPCount++
	as.mu.Unlock()

	as.isDirty.Store(true)
}

// Cache the formatted hour on the write path, including the local UTC offset.
// Callers hold mu; Clear invalidates the cached bucket.
func (as *AnalyticsStore) currentBucketLocked(now time.Time) *HourlyStat {
	_, offset := now.Zone()
	hourID := (now.Unix() + int64(offset)) / 3600
	if as.hourStat != nil && as.hourID == hourID {
		return as.hourStat
	}
	key := now.Format("2006-01-02 15:00")
	stat := as.hourly[key]
	if stat == nil {
		stat = &HourlyStat{Timestamp: key}
		as.hourly[key] = stat
		as.pruneOldBucketsLocked()
	}
	as.hourID, as.hourStat = hourID, stat
	return stat
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
	if topN < 0 {
		topN = 0
	}
	if topN > len(as.topDomains) {
		topN = len(as.topDomains)
	}
	// Keep only N candidates in a min-heap, with the worst candidate at the root.
	domainList := make([]DomainStat, 0, topN)
	for d, c := range as.topDomains {
		if topN == 0 {
			break
		}
		item := DomainStat{Domain: d, Count: c}
		if len(domainList) < topN {
			domainList = append(domainList, item)
			for child := len(domainList) - 1; child > 0; {
				parent := (child - 1) / 2
				if !domainRanksBefore(domainList[parent], domainList[child]) {
					break
				}
				domainList[parent], domainList[child] = domainList[child], domainList[parent]
				child = parent
			}
		} else if domainRanksBefore(item, domainList[0]) {
			domainList[0] = item
			for parent := 0; ; {
				child := 2*parent + 1
				if child >= len(domainList) {
					break
				}
				if child+1 < len(domainList) && domainRanksBefore(domainList[child], domainList[child+1]) {
					child++
				}
				if !domainRanksBefore(domainList[parent], domainList[child]) {
					break
				}
				domainList[parent], domainList[child] = domainList[child], domainList[parent]
				parent = child
			}
		}
	}

	// 2. Sort Hourly Trend
	trendList := make([]HourlyStat, 0, len(as.hourly))
	for _, stat := range as.hourly {
		trendList = append(trendList, *stat)
	}
	as.mu.RUnlock()
	sort.Slice(domainList, func(i, j int) bool {
		return domainRanksBefore(domainList[i], domainList[j])
	})

	sort.Slice(trendList, func(i, j int) bool {
		return trendList[i].Timestamp < trendList[j].Timestamp
	})

	return domainList, trendList
}

func domainRanksBefore(a, b DomainStat) bool {
	if a.Count != b.Count {
		return a.Count > b.Count
	}
	return a.Domain < b.Domain
}

func (as *AnalyticsStore) Clear() {
	if as == nil {
		return
	}
	as.mu.Lock()
	as.topDomains = make(map[string]int64)
	as.hourly = make(map[string]*HourlyStat)
	as.hourStat = nil
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
	as.saveMu.Lock()
	defer as.saveMu.Unlock()
	if !as.isDirty.Swap(false) {
		return
	}

	as.mu.RLock()
	data := AnalyticsData{
		TopDomains: make(map[string]int64, len(as.topDomains)),
		Hourly:     make(map[string]*HourlyStat, len(as.hourly)),
	}
	for domain, count := range as.topDomains {
		data.TopDomains[domain] = count
	}
	for key, stat := range as.hourly {
		clone := *stat
		data.Hourly[key] = &clone
	}
	as.mu.RUnlock()
	encoded, err := json.MarshalIndent(data, "", "  ")

	if err != nil {
		as.isDirty.Store(true)
		return
	}

	tempFile := as.savePath + ".tmp"
	if err := os.WriteFile(tempFile, encoded, 0644); err != nil {
		as.isDirty.Store(true)
		return
	}
	if err := os.Rename(tempFile, as.savePath); err != nil {
		as.isDirty.Store(true)
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
