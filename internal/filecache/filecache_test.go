package filecache

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestCache(t *testing.T, ttl time.Duration) *FileCache {
	t.Helper()
	c, err := New(t.TempDir(), ttl, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNewRejectsEmptyDir(t *testing.T) {
	_, err := New("", time.Hour, nil)
	if err == nil {
		t.Fatal("expected error for empty dir")
	}
}

func TestRoundTrip(t *testing.T) {
	c := newTestCache(t, time.Hour)
	c.Set("k", "value")
	got, ok := c.Get("k")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got != "value" {
		t.Errorf("got %q, want %q", got, "value")
	}
}

func TestGetMissing(t *testing.T) {
	c := newTestCache(t, time.Hour)
	if _, ok := c.Get("nope"); ok {
		t.Error("expected cache miss for unknown key")
	}
}

func TestTTLZeroDisablesCache(t *testing.T) {
	c := newTestCache(t, 0)
	c.Set("k", "value")
	if _, ok := c.Get("k"); ok {
		t.Error("Get should be a miss when ttl=0")
	}

	files, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("ttl=0 should not write any files, found %d", len(files))
	}
}

func TestExpiredEntryIsRemoved(t *testing.T) {
	c := newTestCache(t, 10*time.Millisecond)
	c.Set("k", "value")

	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(c.path("k"), past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if _, ok := c.Get("k"); ok {
		t.Fatal("expected expired entry to miss")
	}
	if _, err := os.Stat(c.path("k")); !os.IsNotExist(err) {
		t.Errorf("expected expired file to be removed, stat err = %v", err)
	}
}

func TestPurgeRemovesOldEntries(t *testing.T) {
	c := newTestCache(t, 10*time.Millisecond)
	c.Set("fresh", "x")
	c.Set("stale", "y")

	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(c.path("stale"), past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	c.Purge()

	if _, err := os.Stat(c.path("stale")); !os.IsNotExist(err) {
		t.Errorf("stale entry should be purged, stat err = %v", err)
	}
	if _, err := os.Stat(c.path("fresh")); err != nil {
		t.Errorf("fresh entry should survive purge, stat err = %v", err)
	}
}

func TestGetRejectsOversizedEntry(t *testing.T) {
	c := newTestCache(t, time.Hour)
	c.maxBytes = 16

	if err := os.WriteFile(c.path("big"), []byte("this is more than sixteen bytes"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, ok := c.Get("big"); ok {
		t.Error("oversized entry should be rejected")
	}
	if _, err := os.Stat(c.path("big")); !os.IsNotExist(err) {
		t.Errorf("oversized entry should be removed, stat err = %v", err)
	}
}

func TestSetPermissions(t *testing.T) {
	c := newTestCache(t, time.Hour)
	c.Set("k", "v")
	info, err := os.Stat(c.path("k"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perms = %v, want 0o600", info.Mode().Perm())
	}
}

func TestNewCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "cache")
	if _, err := New(dir, time.Hour, nil); err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("cache dir should exist: %v", err)
	}
}
