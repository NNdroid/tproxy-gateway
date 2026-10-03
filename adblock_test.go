package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAdBlockSnapshotReplacesAndKeepsOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	ab := NewAdBlocker(AdBlockConfig{})
	if err := os.WriteFile(path, []byte("0.0.0.0 ADS.example.org extra.example.org # comment\n||tracker.example.org^\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ab.loadAll(AdBlockConfig{Files: []string{path}})
	for _, domain := range []string{"ads.example.org", "SUB.ads.example.org.", "extra.example.org", "tracker.example.org"} {
		if !ab.IsBlocked(domain) {
			t.Fatal("not blocked:", domain)
		}
	}
	if ab.IsBlocked("notads.example.org") {
		t.Fatal("label boundary ignored")
	}
	ab.loadAll(AdBlockConfig{Files: []string{path + ".missing"}})
	if !ab.IsBlocked("ads.example.org") {
		t.Fatal("failed refresh removed working snapshot")
	}
	ab.loadAll(AdBlockConfig{Files: []string{path, path + ".missing"}})
	if !ab.IsBlocked("extra.example.org") {
		t.Fatal("incomplete refresh replaced the working snapshot")
	}
	if err := os.WriteFile(path, []byte("new.example.org\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ab.loadAll(AdBlockConfig{Files: []string{path}})
	if ab.IsBlocked("ads.example.org") || !ab.IsBlocked("new.example.org") {
		t.Fatal("snapshot not replaced")
	}
}

func TestAdBlockSlowRefreshDoesNotBlockLookups(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Write([]byte("ads.example.org\n"))
	}))
	defer server.Close()
	defer close(release)
	ab := NewAdBlocker(AdBlockConfig{})
	done := make(chan struct{})
	go func() { ab.loadAll(AdBlockConfig{URLs: []string{server.URL}}); close(done) }()
	<-started
	lookup := make(chan struct{})
	go func() { ab.IsBlocked("example.org"); close(lookup) }()
	select {
	case <-lookup:
	case <-time.After(time.Second):
		t.Fatal("lookup waited for download")
	}
}

func TestAdBlockParserSkipsUnsupportedFilters(t *testing.T) {
	domains := make(map[string]struct{})
	err := parseBlocklist(strings.NewReader("! comment\n@@||allow.example^\nhttps://bad.example/path\n||ads.example^\n"), domains)
	if err != nil || len(domains) != 1 {
		t.Fatalf("unexpected filters: %v, %v", domains, err)
	}
}
