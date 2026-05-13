package extractor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memCache struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemCache() *memCache { return &memCache{m: make(map[string]string)} }

func (c *memCache) Get(k string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok
}

func (c *memCache) Set(k, v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = v
}

func newExtractor(t *testing.T, cache Cache) *Extractor {
	t.Helper()
	return New(Config{
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		UserAgent:  "test/1.0",
		MaxBytes:   1 << 20,
		Cache:      cache,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestSanitizeStripsScripts(t *testing.T) {
	e := newExtractor(t, nil)
	got := e.Sanitize(`<p>Hello</p><script>alert(1)</script>`)
	if strings.Contains(got, "<script>") || strings.Contains(got, "alert") {
		t.Errorf("expected script to be stripped, got %q", got)
	}
	if !strings.Contains(got, "<p>Hello</p>") {
		t.Errorf("expected benign HTML to remain, got %q", got)
	}
}

func TestSanitizeTrimsWhitespace(t *testing.T) {
	e := newExtractor(t, nil)
	got := e.Sanitize("   hello   ")
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

const articleHTML = `<!doctype html>
<html><head><title>Test</title></head>
<body>
<article>
<h1>Hello World</h1>
<p>This is the first paragraph of a substantial article that should be picked up by readability. It contains enough textual content to clear any density thresholds the algorithm applies.</p>
<p>This is the second paragraph, also with plenty of words so the heuristic considers this the main body of the page. We add more sentences. And more sentences. And still more sentences so it is unambiguous.</p>
<p>A third paragraph rounds it out with additional prose so the extractor has no doubt about which element holds the article content.</p>
<script>alert('xss-from-article')</script>
</article>
</body></html>`

func TestExtractHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, articleHTML)
	}))
	defer srv.Close()

	e := newExtractor(t, nil)
	got, err := e.Extract(context.Background(), srv.URL+"/article")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got == "" {
		t.Fatal("expected non-empty content")
	}
	if strings.Contains(got, "<script>") || strings.Contains(got, "xss-from-article") {
		t.Errorf("expected script to be stripped from extracted content, got %q", got)
	}
}

func TestExtractRejectsNonHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"hello":"world"}`)
	}))
	defer srv.Close()

	e := newExtractor(t, nil)
	_, err := e.Extract(context.Background(), srv.URL+"/data")
	if !errors.Is(err, ErrUnsupportedContent) {
		t.Errorf("expected ErrUnsupportedContent, got %v", err)
	}
}

func TestExtractRejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := newExtractor(t, nil)
	_, err := e.Extract(context.Background(), srv.URL+"/x")
	if !errors.Is(err, ErrUpstreamStatus) {
		t.Errorf("expected ErrUpstreamStatus, got %v", err)
	}
}

func TestExtractRejectsInvalidURL(t *testing.T) {
	e := newExtractor(t, nil)
	_, err := e.Extract(context.Background(), "not-a-url")
	if !errors.Is(err, ErrInvalidURL) {
		t.Errorf("expected ErrInvalidURL, got %v", err)
	}
	_, err = e.Extract(context.Background(), "ftp://example.com/x")
	if !errors.Is(err, ErrInvalidURL) {
		t.Errorf("expected ErrInvalidURL for non-http scheme, got %v", err)
	}
}

func TestExtractUsesCache(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, articleHTML)
	}))
	defer srv.Close()

	cache := newMemCache()
	e := newExtractor(t, cache)

	if _, err := e.Extract(context.Background(), srv.URL+"/a"); err != nil {
		t.Fatalf("first Extract: %v", err)
	}
	if _, err := e.Extract(context.Background(), srv.URL+"/a"); err != nil {
		t.Fatalf("second Extract: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("expected 1 upstream hit (second call served from cache), got %d", got)
	}
}

func TestExtractNegativeCachesOn4xx(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "gone", http.StatusGone)
	}))
	defer srv.Close()

	cache := newMemCache()
	e := newExtractor(t, cache)

	if _, err := e.Extract(context.Background(), srv.URL+"/dead"); !errors.Is(err, ErrUpstreamStatus) {
		t.Fatalf("first: expected ErrUpstreamStatus, got %v", err)
	}
	if _, err := e.Extract(context.Background(), srv.URL+"/dead"); !errors.Is(err, ErrUpstreamStatus) {
		t.Fatalf("second: expected ErrUpstreamStatus from cache, got %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("expected 1 upstream hit (negative cache), got %d", got)
	}
}

func TestExtractDoesNotNegativeCacheOn5xx(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cache := newMemCache()
	e := newExtractor(t, cache)

	for i := 0; i < 2; i++ {
		if _, err := e.Extract(context.Background(), srv.URL+"/x"); !errors.Is(err, ErrUpstreamStatus) {
			t.Fatalf("call %d: expected ErrUpstreamStatus, got %v", i, err)
		}
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("5xx should not be negative-cached; expected 2 upstream hits, got %d", got)
	}
}

func TestExtractNegativeCachesOnUnsupportedContent(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	cache := newMemCache()
	e := newExtractor(t, cache)

	for i := 0; i < 3; i++ {
		if _, err := e.Extract(context.Background(), srv.URL+"/x"); !errors.Is(err, ErrUnsupportedContent) {
			t.Fatalf("call %d: expected ErrUnsupportedContent, got %v", i, err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("expected unsupported-content to be negative-cached, got %d upstream hits", got)
	}
}

func TestExtractCachesPostRedirectURL(t *testing.T) {
	var canonHits, redirHits atomic.Int64

	canon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		canonHits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, articleHTML)
	}))
	defer canon.Close()

	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirHits.Add(1)
		http.Redirect(w, r, canon.URL+"/canon", http.StatusFound)
	}))
	defer redir.Close()

	cache := newMemCache()
	e := newExtractor(t, cache)

	if _, err := e.Extract(context.Background(), redir.URL+"/short"); err != nil {
		t.Fatalf("via redirect: %v", err)
	}
	if _, err := e.Extract(context.Background(), canon.URL+"/canon"); err != nil {
		t.Fatalf("via canonical: %v", err)
	}
	if got := canonHits.Load(); got != 1 {
		t.Errorf("canonical should be hit only once (second call cached), got %d", got)
	}
	if got := redirHits.Load(); got != 1 {
		t.Errorf("redirector should be hit only once, got %d", got)
	}
}
