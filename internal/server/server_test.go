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

func TestServesXMLFile(t *testing.T) {
	s, dir := newTestServer(t)
	if err := os.WriteFile(filepath.Join(dir, "example.xml"), []byte("<rss/>"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	rec := doRequest(t, s.Routes(), "/example.xml")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/rss+xml") {
		t.Errorf("content-type = %q, want rss+xml", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("X-Content-Type-Options header missing")
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
