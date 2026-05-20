package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rss-fulltext/internal/generator"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	tr := generator.NewTracker()
	tr.Init("example", generator.Status{
		Name:      "example",
		SourceURL: "https://example.com/feed",
		FileURL:   "/example.xml",
		Interval:  "1h",
	})
	s := New(Config{
		OutputDir: dir,
		Tracker:   tr,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return s, dir
}

func doRequest(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s.Routes(), "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("body = %q, want 'ok'", rec.Body.String())
	}
}

func TestFeedsJSON(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s.Routes(), "/feeds.json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want json", ct)
	}
	var body struct {
		Feeds []generator.Status `json:"feeds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Feeds) != 1 || body.Feeds[0].Name != "example" {
		t.Errorf("unexpected feeds payload: %+v", body)
	}
}

func TestServesFeedFiles(t *testing.T) {
	s, dir := newTestServer(t)
	for _, name := range []string{"example.xml", "example.atom", "example.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("payload"), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}

	cases := []struct {
		path string
		ct   string
	}{
		{"/example.xml", "application/rss+xml"},
		{"/example.atom", "application/atom+xml"},
		{"/example.json", "application/feed+json"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := doRequest(t, s.Routes(), tc.path)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.ct) {
				t.Errorf("content-type = %q, want %q", ct, tc.ct)
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Error("X-Content-Type-Options header missing")
			}
		})
	}
}

func TestMetricsEndpoint(t *testing.T) {
	dir := t.TempDir()
	tr := generator.NewTracker()
	tr.Init("example", generator.Status{Name: "example"})
	metricsHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("rss_fulltext_test 1\n"))
	})
	s := New(Config{
		OutputDir: dir,
		Tracker:   tr,
		Metrics:   metricsHandler,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	rec := doRequest(t, s.Routes(), "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rss_fulltext_test 1") {
		t.Errorf("metrics body unexpected: %s", rec.Body.String())
	}
}

func TestMetricsEndpointDisabledWhenNil(t *testing.T) {
	// When no metrics handler is configured, /metrics should not be mounted
	// and the fallback static handler will reject it (no .xml extension).
	s, _ := newTestServer(t)
	rec := doRequest(t, s.Routes(), "/metrics")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestRootIs404(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s.Routes(), "/")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestRejectsBadPaths(t *testing.T) {
	s, dir := newTestServer(t)
	if err := os.WriteFile(filepath.Join(dir, "example.xml"), []byte("<rss/>"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cases := []string{
		"/no-extension",      // not .xml
		"/.hidden.xml",       // leading dot
		"/sub/example.xml",   // subpath
		"/example.txt",       // wrong extension
		"/missing.xml",       // .xml but file doesn't exist
	}
	for _, target := range cases {
		t.Run(target, func(t *testing.T) {
			rec := doRequest(t, s.Routes(), target)
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
		})
	}
}
