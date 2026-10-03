package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func newBenchmarkAnalytics(n int) *AnalyticsStore {
	as := &AnalyticsStore{topDomains: make(map[string]int64, n), hourly: make(map[string]*HourlyStat)}
	for i := range n {
		as.topDomains[fmt.Sprintf("host-%06d.example", i)] = int64(i % 1000)
	}
	return as
}

func BenchmarkAnalyticsSummary(b *testing.B) {
	for _, n := range []int{1000, 100000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			as := newBenchmarkAnalytics(n)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				as.GetSummary(20)
			}
		})
	}
}

func BenchmarkRecordDNSQuery(b *testing.B) {
	as := newBenchmarkAnalytics(1000)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		as.RecordDNSQuery("host-000500.example.")
	}
}

func BenchmarkLogPush(b *testing.B) {
	buffer := &LogRingBuffer{maxSize: 500, entries: make([]LogEntry, 0, 500)}
	for range 500 {
		buffer.Push("INFO", "gateway connection opened")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		buffer.Push("INFO", "gateway connection opened")
	}
}

func BenchmarkAdBlockLookup(b *testing.B) {
	ab := NewAdBlocker(AdBlockConfig{})
	path := filepath.Join(b.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte("0.0.0.0 ads.example.org\n"), 0600); err != nil {
		b.Fatal(err)
	}
	ab.loadAll(AdBlockConfig{Files: []string{path}})
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ab.IsBlocked("assets.example.org")
		}
	})
}
