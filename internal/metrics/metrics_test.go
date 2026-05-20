package metrics

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCounterAndGaugeOutput(t *testing.T) {
	m := New()
	m.RecordRefresh("tc", "ok", 1500*time.Millisecond)
	m.RecordRefresh("tc", "ok", 2*time.Second)
	m.RecordRefresh("tc", "fail", 500*time.Millisecond)
	m.RecordRefreshSuccess("tc", time.Unix(1700000000, 0), 42)
	m.RecordExtract("ok")
	m.RecordExtract("ok")
	m.RecordExtract("cache_hit")

	var buf bytes.Buffer
	if err := m.registry.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	got := buf.String()

	wants := []string{
		`# HELP rss_fulltext_refresh_total`,
		`# TYPE rss_fulltext_refresh_total counter`,
		`rss_fulltext_refresh_total{feed="tc",outcome="ok"} 2`,
		`rss_fulltext_refresh_total{feed="tc",outcome="fail"} 1`,
		`# TYPE rss_fulltext_refresh_duration_seconds gauge`,
		`rss_fulltext_refresh_duration_seconds{feed="tc"} 0.5`,
		`rss_fulltext_refresh_items{feed="tc"} 42`,
		`rss_fulltext_refresh_last_success_timestamp_seconds{feed="tc"} 1700000000`,
		`rss_fulltext_extract_total{outcome="ok"} 2`,
		`rss_fulltext_extract_total{outcome="cache_hit"} 1`,
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q\n--- got ---\n%s", w, got)
		}
	}
}

func TestHandlerServesPlainText(t *testing.T) {
	m := New()
	m.RecordExtract("ok")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain*", ct)
	}
	if !strings.Contains(rec.Body.String(), "rss_fulltext_extract_total") {
		t.Errorf("body missing extract counter: %s", rec.Body.String())
	}
}

func TestLabelEscaping(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("test_total", "with \\back and\nnewline.", "feed")
	c.Inc(`weird"\name`)

	var buf bytes.Buffer
	if err := r.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	got := buf.String()

	if !strings.Contains(got, `# HELP test_total with \\back and\nnewline.`) {
		t.Errorf("help text not escaped: %s", got)
	}
	if !strings.Contains(got, `test_total{feed="weird\"\\name"} 1`) {
		t.Errorf("label value not escaped: %s", got)
	}
}

func TestCounterNegativeAddPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on negative counter Add")
		}
	}()
	r := NewRegistry()
	c := r.NewCounter("t", "h")
	c.Add(-1)
}

func TestMismatchedRegisterPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on mismatched re-registration")
		}
	}()
	r := NewRegistry()
	_ = r.NewCounter("t", "h", "feed")
	_ = r.NewCounter("t", "h", "feed", "outcome") // different shape
}

func TestNilSinkSafe(t *testing.T) {
	var m *Metrics
	// Calls on a nil receiver must not panic; this matches how callers may
	// be configured (Metrics: nil).
	m.RecordRefresh("x", "ok", time.Second)
	m.RecordRefreshSuccess("x", time.Now(), 1)
	m.RecordExtract("ok")
	// Handler() on nil returns a 404 handler; verify it doesn't panic.
	h := m.Handler()
	if h == nil {
		t.Fatal("Handler() returned nil")
	}
}

func TestConcurrentUpdatesAreCounted(t *testing.T) {
	m := New()
	const N = 200
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			m.RecordExtract("ok")
		}()
	}
	wg.Wait()

	var buf bytes.Buffer
	if err := m.registry.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if !strings.Contains(buf.String(), `rss_fulltext_extract_total{outcome="ok"} 200`) {
		t.Errorf("expected 200 ok extracts, got:\n%s", buf.String())
	}
}
