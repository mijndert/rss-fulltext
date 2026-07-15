package config

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestWatchDetectsChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "feeds.yaml")
	writeFile(t, path, "feeds:\n  - name: a\n    url: https://a.test/rss\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	changes := make(chan *File, 4)
	go Watch(ctx, path, 10*time.Millisecond, func(f *File) { changes <- f }, discardLogger())

	// A change to the file should trigger a reload with the new contents. The
	// first write can lose the race with the watcher's initial os.Stat: if it
	// lands before the baseline is captured it becomes the baseline and no change
	// is ever seen. Re-apply the edit until the reload arrives; each rewrite
	// advances the file's mtime, so a later poll is guaranteed to catch it.
	updated := "feeds:\n  - name: a\n    url: https://a.test/rss\n  - name: b\n    url: https://b.test/rss\n"
	writeFile(t, path, updated)

	deadline := time.After(2 * time.Second)
	rewrite := time.NewTicker(50 * time.Millisecond)
	defer rewrite.Stop()
	for {
		select {
		case f := <-changes:
			if len(f.Feeds) != 2 {
				t.Fatalf("reloaded feeds = %d, want 2", len(f.Feeds))
			}
			return
		case <-rewrite.C:
			writeFile(t, path, updated)
		case <-deadline:
			t.Fatal("timed out waiting for reload")
		}
	}
}

func TestWatchIgnoresInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "feeds.yaml")
	writeFile(t, path, "feeds:\n  - name: a\n    url: https://a.test/rss\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	changes := make(chan *File, 4)
	go Watch(ctx, path, 10*time.Millisecond, func(f *File) { changes <- f }, discardLogger())

	// Uppercase name fails validation; onChange must NOT fire and the watcher
	// must keep running rather than error out.
	writeFile(t, path, "feeds:\n  - name: BadName\n    url: https://a.test/rss\n")
	select {
	case f := <-changes:
		t.Fatalf("onChange fired for invalid config: %+v", f)
	case <-time.After(200 * time.Millisecond):
		// expected: no reload
	}

	// A subsequent valid edit is still picked up.
	writeFile(t, path, "feeds:\n  - name: c\n    url: https://c.test/rss\n")
	select {
	case f := <-changes:
		if len(f.Feeds) != 1 || f.Feeds[0].Name != "c" {
			t.Fatalf("reloaded = %+v, want single feed c", f.Feeds)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reload after fixing the file")
	}
}

func TestWatchZeroIntervalReturns(t *testing.T) {
	done := make(chan struct{})
	go func() {
		Watch(context.Background(), "irrelevant", 0, func(*File) {}, discardLogger())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Watch with zero interval should return immediately")
	}
}
