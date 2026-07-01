package generator

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"rss-fulltext/internal/config"
)

type stubExtractor struct{}

func (stubExtractor) Extract(context.Context, string) (string, error) { return "<p>body</p>", nil }
func (stubExtractor) Sanitize(s string) string                        { return s }

const testRSS = `<?xml version="1.0"?>
<rss version="2.0"><channel><title>T</title><link>https://example.test</link>
<item><title>i</title><link>https://example.test/a</link></item>
</channel></rss>`

func testManager(t *testing.T) (*Manager, string) {
	t.Helper()
	out := t.TempDir()
	m := NewManager(ManagerConfig{
		HTTPClient: http.DefaultClient,
		Extractor:  stubExtractor{},
		OutputDir:  out,
		Tracker:    NewTracker(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return m, out
}

func feed(name, url string) config.Feed {
	return config.Feed{Name: name, URL: url, Interval: time.Hour}
}

// eventually polls fn until it returns true or the deadline passes.
func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestApplyStartsRemovesAndReplaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, testRSS)
	}))
	defer srv.Close()

	m, out := testManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer m.Shutdown()

	m.Apply(ctx, []config.Feed{feed("a", srv.URL), feed("b", srv.URL)})

	eventually(t, func() bool {
		return fileExists(filepath.Join(out, "a.xml")) && fileExists(filepath.Join(out, "b.xml"))
	})
	if got := len(m.cfg.Tracker.Snapshot()); got != 2 {
		t.Fatalf("tracker snapshot = %d, want 2", got)
	}

	// Swap b for c: b's worker and files go away, c appears, a is untouched.
	m.mu.Lock()
	aHandle := m.workers["a"]
	m.mu.Unlock()

	m.Apply(ctx, []config.Feed{feed("a", srv.URL), feed("c", srv.URL)})

	eventually(t, func() bool {
		return fileExists(filepath.Join(out, "c.xml")) && !fileExists(filepath.Join(out, "b.xml"))
	})

	names := m.Names()
	sort.Strings(names)
	if len(names) != 2 || names[0] != "a" || names[1] != "c" {
		t.Fatalf("running workers = %v, want [a c]", names)
	}
	m.mu.Lock()
	if m.workers["a"] != aHandle {
		t.Error("worker a was restarted but should have been left untouched")
	}
	m.mu.Unlock()
	if !fileExists(filepath.Join(out, "a.xml")) {
		t.Error("a.xml should still exist")
	}
}

func TestApplyIdempotent(t *testing.T) {
	m, _ := testManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer m.Shutdown()

	feeds := []config.Feed{feed("a", "https://a.invalid/rss")}
	m.Apply(ctx, feeds)
	m.mu.Lock()
	first := m.workers["a"]
	m.mu.Unlock()

	m.Apply(ctx, feeds)
	m.mu.Lock()
	second := m.workers["a"]
	m.mu.Unlock()

	if first != second {
		t.Fatal("re-applying an unchanged feed set restarted the worker")
	}
}

func TestApplyRestartsChangedFeed(t *testing.T) {
	m, _ := testManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer m.Shutdown()

	m.Apply(ctx, []config.Feed{feed("a", "https://a.invalid/one")})
	m.mu.Lock()
	first := m.workers["a"]
	m.mu.Unlock()

	m.Apply(ctx, []config.Feed{feed("a", "https://a.invalid/two")})
	m.mu.Lock()
	second := m.workers["a"]
	m.mu.Unlock()

	if first == second {
		t.Fatal("changing a feed's URL should have restarted its worker")
	}
}

func TestShutdownStopsAll(t *testing.T) {
	m, _ := testManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.Apply(ctx, []config.Feed{feed("a", "https://a.invalid/rss"), feed("b", "https://b.invalid/rss")})
	m.Shutdown()
	if names := m.Names(); len(names) != 0 {
		t.Fatalf("after shutdown running workers = %v, want none", names)
	}
}

func TestFeedChanged(t *testing.T) {
	base := feed("a", "https://a.test/rss")
	cases := []struct {
		name string
		b    config.Feed
		want bool
	}{
		{"identical", base, false},
		{"url differs", feed("a", "https://a.test/other"), true},
		{"interval differs", config.Feed{Name: "a", URL: base.URL, Interval: 30 * time.Minute}, true},
		{"title differs", config.Feed{Name: "a", URL: base.URL, Interval: time.Hour, Title: "New"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := feedChanged(base, tc.b); got != tc.want {
				t.Errorf("feedChanged = %v, want %v", got, tc.want)
			}
		})
	}
}
