package generator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/microcosm-cc/bluemonday"

	"rss-fulltext/internal/config"
)

func TestFirstNonEmpty(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"", "x", "y"}, "x"},
		{[]string{"", ""}, ""},
		{[]string{"   ", "x"}, "x"},
		{[]string{"first"}, "first"},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := firstNonEmpty(tc.in...); got != tc.want {
			t.Errorf("firstNonEmpty(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOrZero(t *testing.T) {
	if !orZero(nil).IsZero() {
		t.Error("orZero(nil) should be zero time")
	}
	now := time.Now()
	if !orZero(&now).Equal(now) {
		t.Error("orZero(&now) should be now")
	}
}

func TestOrNow(t *testing.T) {
	if orNow(nil).IsZero() {
		t.Error("orNow(nil) should not be zero")
	}
	zero := time.Time{}
	if orNow(&zero).IsZero() {
		t.Error("orNow(zero) should fall back to now")
	}
	specific := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	if !orNow(&specific).Equal(specific) {
		t.Error("orNow(specific) should return that time")
	}
}

func TestTrackerSnapshot(t *testing.T) {
	tr := NewTracker()
	tr.Init("a", Status{Name: "a", Title: "A"})
	tr.Init("b", Status{Name: "b", Title: "B"})

	snap := tr.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	names := map[string]bool{}
	for _, s := range snap {
		names[s.Name] = true
	}
	if !names["a"] || !names["b"] {
		t.Errorf("missing names in snapshot: %v", names)
	}
}

func TestTrackerConcurrentAccess(t *testing.T) {
	tr := NewTracker()
	tr.Init("a", Status{Name: "a"})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			tr.update("a", func(s *Status) { s.ItemCount++ })
		}()
		go func() {
			defer wg.Done()
			_ = tr.Snapshot()
		}()
	}
	wg.Wait()

	snap := tr.Snapshot()
	if len(snap) != 1 || snap[0].ItemCount != 50 {
		t.Errorf("item_count = %d, want 50", snap[0].ItemCount)
	}
}

type fakeExtractor struct {
	content   string
	err       error
	sanitizer *bluemonday.Policy
}

func (f *fakeExtractor) Extract(_ context.Context, _ string) (string, error) {
	return f.content, f.err
}

func (f *fakeExtractor) Sanitize(s string) string {
	if f.sanitizer == nil {
		return s
	}
	return strings.TrimSpace(f.sanitizer.Sanitize(s))
}

const sampleRSS = `<?xml version="1.0"?>
<rss version="2.0"><channel>
<title>Sample</title>
<link>https://example.test/</link>
<description>desc</description>
<item>
<title>Item One</title>
<link>https://example.test/one</link>
<description>summary one</description>
</item>
<item>
<title>Item Two</title>
<link>https://example.test/two</link>
<description>summary two</description>
</item>
</channel></rss>`

const emptyRSS = `<?xml version="1.0"?>
<rss version="2.0"><channel>
<title>Empty</title>
<link>https://example.test/</link>
<description>nothing</description>
</channel></rss>`

func newWorker(t *testing.T, feedURL, outputDir string, ext Extractor) *Worker {
	t.Helper()
	return NewWorker(WorkerConfig{
		Feed: config.Feed{
			Name:     "sample",
			URL:      feedURL,
			Interval: time.Hour,
		},
		HTTPClient:   &http.Client{Timeout: 5 * time.Second},
		Extractor:    ext,
		OutputDir:    outputDir,
		MaxFeedBytes: 1 << 20,
		MaxItems:     50,
		Concurrency:  2,
		FeedTimeout:  5 * time.Second,
		Tracker:      NewTracker(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestRefreshWritesFeed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, sampleRSS)
	}))
	defer srv.Close()

	dir := t.TempDir()
	w := newWorker(t, srv.URL, dir, &fakeExtractor{content: "extracted-body-marker"})
	w.cfg.Tracker.Init(w.cfg.Feed.Name, Status{Name: w.cfg.Feed.Name})

	if err := w.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "sample.xml"))
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !strings.Contains(string(body), "Item One") {
		t.Errorf("output missing item title: %s", body)
	}
	if !strings.Contains(string(body), "extracted-body-marker") {
		t.Errorf("output missing extracted content (Extract must reach the rendered XML): %s", body)
	}
}

func TestRefreshWritesAllFormats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, sampleRSS)
	}))
	defer srv.Close()

	dir := t.TempDir()
	w := newWorker(t, srv.URL, dir, &fakeExtractor{content: "extracted-body-marker"})
	w.cfg.Tracker.Init(w.cfg.Feed.Name, Status{Name: w.cfg.Feed.Name})

	if err := w.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	cases := []struct {
		ext      string
		contains []string
	}{
		{".xml", []string{"<rss", "Item One", "extracted-body-marker"}},
		{".atom", []string{"<feed", "Item One", "extracted-body-marker"}},
		{".json", []string{`"version"`, "Item One", "extracted-body-marker"}},
	}
	for _, tc := range cases {
		t.Run(tc.ext, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(dir, "sample"+tc.ext))
			if err != nil {
				t.Fatalf("read %s output: %v", tc.ext, err)
			}
			for _, want := range tc.contains {
				if !strings.Contains(string(body), want) {
					t.Errorf("%s output missing %q. body=%s", tc.ext, want, body)
				}
			}
		})
	}

	// No stray dotfiles (temp files) should be left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("unexpected leftover temp file: %s", e.Name())
		}
	}
}

const maliciousRSS = `<?xml version="1.0"?>
<rss version="2.0"><channel>
<title>Malicious</title>
<link>https://example.test/</link>
<description>desc</description>
<item>
<title>Item</title>
<link>https://example.test/one</link>
<description><![CDATA[<p>hello</p><script>alert('xss-from-description')</script>]]></description>
</item>
</channel></rss>`

func TestRefreshSanitizesUpstreamDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, maliciousRSS)
	}))
	defer srv.Close()

	dir := t.TempDir()
	ext := &fakeExtractor{
		content:   "safe-extracted",
		sanitizer: bluemonday.UGCPolicy(),
	}
	w := newWorker(t, srv.URL, dir, ext)
	w.cfg.Tracker.Init(w.cfg.Feed.Name, Status{Name: w.cfg.Feed.Name})

	if err := w.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "sample.xml"))
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	got := string(body)
	if strings.Contains(got, "<script>") || strings.Contains(got, "xss-from-description") {
		t.Errorf("malicious script not stripped from output: %s", got)
	}
}

func TestRefreshExtractFailureFallsBackToSanitizedDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, maliciousRSS)
	}))
	defer srv.Close()

	dir := t.TempDir()
	ext := &fakeExtractor{
		err:       errors.New("extract boom"),
		sanitizer: bluemonday.UGCPolicy(),
	}
	w := newWorker(t, srv.URL, dir, ext)
	w.cfg.Tracker.Init(w.cfg.Feed.Name, Status{Name: w.cfg.Feed.Name})

	if err := w.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "sample.xml"))
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "hello") {
		t.Errorf("expected sanitized description as fallback content, got: %s", got)
	}
	if strings.Contains(got, "<script>") || strings.Contains(got, "xss-from-description") {
		t.Errorf("extract-failure fallback path leaked unsanitized HTML: %s", got)
	}
}

func TestRefreshSkipsWriteWhenEmptyAndPreviousExists(t *testing.T) {
	dir := t.TempDir()
	prev := filepath.Join(dir, "sample.xml")
	if err := os.WriteFile(prev, []byte("<rss>previous</rss>"), 0o644); err != nil {
		t.Fatalf("seed previous: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, emptyRSS)
	}))
	defer srv.Close()

	w := newWorker(t, srv.URL, dir, &fakeExtractor{content: ""})
	w.cfg.Tracker.Init(w.cfg.Feed.Name, Status{Name: w.cfg.Feed.Name})

	if err := w.Refresh(context.Background()); err == nil {
		t.Fatal("expected Refresh to surface error on empty upstream")
	}

	body, err := os.ReadFile(prev)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(body) != "<rss>previous</rss>" {
		t.Errorf("previous output was overwritten: %q", body)
	}

	snap := w.cfg.Tracker.Snapshot()
	if len(snap) != 1 || snap[0].LastRefreshOK {
		t.Errorf("expected LastRefreshOK=false, got %+v", snap)
	}
	if !strings.Contains(snap[0].LastError, "0 items") {
		t.Errorf("expected 'no items' error, got %q", snap[0].LastError)
	}
}

func TestRefreshRefusesEmptyOnColdStart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, emptyRSS)
	}))
	defer srv.Close()

	dir := t.TempDir()
	w := newWorker(t, srv.URL, dir, &fakeExtractor{})
	w.cfg.Tracker.Init(w.cfg.Feed.Name, Status{Name: w.cfg.Feed.Name})

	err := w.Refresh(context.Background())
	if err == nil {
		t.Fatal("cold-start with 0 items should error")
	}
	if !strings.Contains(err.Error(), "no previous file") {
		t.Errorf("expected 'no previous file' in error, got %q", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "sample.xml")); !os.IsNotExist(statErr) {
		t.Error("cold-start 0-items must not write a file")
	}
}

func TestRefreshOverwritesPastStaleness(t *testing.T) {
	dir := t.TempDir()
	prev := filepath.Join(dir, "sample.xml")
	if err := os.WriteFile(prev, []byte("<rss>old</rss>"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Backdate the file well past the staleness budget.
	stale := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(prev, stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, emptyRSS)
	}))
	defer srv.Close()

	w := newWorker(t, srv.URL, dir, &fakeExtractor{})
	w.cfg.MaxStaleness = time.Hour
	w.cfg.Tracker.Init(w.cfg.Feed.Name, Status{Name: w.cfg.Feed.Name})

	if err := w.Refresh(context.Background()); err == nil {
		t.Fatal("expected error from staleness overwrite")
	}
	body, err := os.ReadFile(prev)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if strings.Contains(string(body), "old") {
		t.Errorf("stale content should have been overwritten, got: %s", body)
	}
	if !strings.Contains(string(body), "<rss") {
		t.Errorf("expected fresh empty RSS file to be written, got: %s", body)
	}
}

func TestRefreshSetsLastSuccessAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, sampleRSS)
	}))
	defer srv.Close()

	dir := t.TempDir()
	w := newWorker(t, srv.URL, dir, &fakeExtractor{content: "<p>body</p>"})
	w.cfg.Tracker.Init(w.cfg.Feed.Name, Status{Name: w.cfg.Feed.Name})

	if err := w.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap := w.cfg.Tracker.Snapshot()
	if len(snap) != 1 || snap[0].LastSuccessAt.IsZero() {
		t.Errorf("expected LastSuccessAt to be set on success, got %+v", snap)
	}
	if !snap[0].LastRefreshOK {
		t.Errorf("expected LastRefreshOK=true, got %+v", snap)
	}
}

func TestTrackerUpdateUnknownNameIsNoOp(t *testing.T) {
	tr := NewTracker()
	tr.update("never-initialised", func(s *Status) { s.ItemCount = 99 })
	if got := tr.Snapshot(); len(got) != 0 {
		t.Errorf("update on unknown name should be a no-op, got %+v", got)
	}
}

func TestRefreshFailsOnUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	w := newWorker(t, srv.URL, dir, &fakeExtractor{})
	w.cfg.Tracker.Init(w.cfg.Feed.Name, Status{Name: w.cfg.Feed.Name})

	if err := w.Refresh(context.Background()); err == nil {
		t.Fatal("expected error on 500")
	}
	if _, err := os.Stat(filepath.Join(dir, "sample.xml")); !os.IsNotExist(err) {
		t.Error("no output should be written on upstream failure")
	}
}
