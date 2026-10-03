package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestAnalyticsSummaryMatchesFullSort(t *testing.T) {
	as := newBenchmarkAnalytics(5000)
	as.hourly["2026-10-03 12:00"] = &HourlyStat{Timestamp: "2026-10-03 12:00", DNSCount: 2}
	as.hourly["2026-10-03 11:00"] = &HourlyStat{Timestamp: "2026-10-03 11:00", DNSCount: 1}
	want := make([]DomainStat, 0, len(as.topDomains))
	for domain, count := range as.topDomains {
		want = append(want, DomainStat{domain, count})
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].Count == want[j].Count {
			return want[i].Domain < want[j].Domain
		}
		return want[i].Count > want[j].Count
	})
	for _, n := range []int{-1, 0, 1, 20, 500, 1000, 2000, 5000, 6000} {
		top, trend := as.GetSummary(n)
		limit := max(0, min(n, len(want)))
		if !reflect.DeepEqual(top, want[:limit]) {
			t.Fatalf("top %d differs from full sort", n)
		}
		if len(trend) != 2 || trend[0].DNSCount != 1 {
			t.Fatal("trend order changed")
		}
	}
}

func TestAnalyticsBucketRolloverAndClear(t *testing.T) {
	as := newBenchmarkAnalytics(0)
	zone := time.FixedZone("quarter-hour", 5*3600+45*60)
	first := time.Date(2026, 10, 3, 10, 59, 0, 0, zone)
	as.currentBucketLocked(first).DNSCount++
	as.currentBucketLocked(first.Add(time.Minute)).TCPCount++
	if len(as.hourly) != 2 {
		t.Fatal("local hour boundary did not create a new bucket")
	}
	as.savePath = filepath.Join(t.TempDir(), "analytics.json")
	as.Clear()
	as.RecordDNSQuery("EXAMPLE.org.")
	_, trend := as.GetSummary(20)
	if len(trend) != 1 || trend[0].DNSCount != 1 {
		t.Fatal("clear retained the cached bucket")
	}
}

func TestAnalyticsConcurrentRecordSaveAndSummary(t *testing.T) {
	as := newBenchmarkAnalytics(0)
	as.savePath = filepath.Join(t.TempDir(), "analytics.json")
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				as.RecordDNSQuery("example.org.")
				as.GetSummary(20)
			}
		})
	}
	wg.Go(func() {
		for range 10 {
			as.saveToFileIfDirty()
		}
	})
	wg.Wait()
	as.saveToFileIfDirty()
	encoded, err := os.ReadFile(as.savePath)
	if err != nil {
		t.Fatal(err)
	}
	var data AnalyticsData
	if err := json.Unmarshal(encoded, &data); err != nil {
		t.Fatal(err)
	}
	if data.TopDomains["example.org"] != 800 {
		t.Fatalf("persisted count = %d", data.TopDomains["example.org"])
	}
	if as.isDirty.Load() {
		t.Fatal("successful save left dirty flag set")
	}
}

func TestAnalyticsFailedSaveRetainsDirty(t *testing.T) {
	as := newBenchmarkAnalytics(0)
	as.savePath = filepath.Join(t.TempDir(), "missing", "analytics.json")
	as.RecordDNSQuery("example.org")
	as.saveToFileIfDirty()
	if !as.isDirty.Load() {
		t.Fatal("failed save lost pending data")
	}
}
