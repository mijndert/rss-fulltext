package extractor

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// TestBBCManualExtract is a one-shot E2E that runs the full extractor pipeline
// against a local BBC sample. Skipped unless BBC_HTML_PATH is set, e.g.:
//
//	BBC_HTML_PATH=/tmp/bbc-article.html go test ./internal/extractor/ -run BBCManual -v
//
// Writes the post-pipeline HTML to BBC_OUT_PATH (default /tmp/bbc-after.html)
// for manual inspection.
func TestBBCManualExtract(t *testing.T) {
	src := os.Getenv("BBC_HTML_PATH")
	if src == "" {
		t.Skip("set BBC_HTML_PATH to run")
	}
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	e := New(Config{
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
		UserAgent:  "test/1.0",
		MaxBytes:   10 << 20,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	got, err := e.Extract(context.Background(), srv.URL+"/article")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	out := os.Getenv("BBC_OUT_PATH")
	if out == "" {
		out = "/tmp/bbc-after.html"
	}
	if err := os.WriteFile(out, []byte(got), 0o644); err != nil {
		t.Fatalf("write %s: %v", out, err)
	}
	t.Logf("wrote %d bytes to %s", len(got), out)
}
