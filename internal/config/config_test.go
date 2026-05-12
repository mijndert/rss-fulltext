package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "feeds.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	p := writeYAML(t, `
default_interval: 30m
feeds:
  - name: example
    url: https://example.com/feed.xml
    title: Example
    interval: 1h
  - name: minimal
    url: https://minimal.test/rss
`)
	f, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.Feeds) != 2 {
		t.Fatalf("expected 2 feeds, got %d", len(f.Feeds))
	}
	if f.DefaultInterval != 30*time.Minute {
		t.Errorf("default_interval = %s, want 30m", f.DefaultInterval)
	}
	if f.Feeds[1].Interval != 30*time.Minute {
		t.Errorf("minimal interval = %s, want 30m (inherited from default)", f.Feeds[1].Interval)
	}
}

func TestLoadDefaultsAppliedWhenOmitted(t *testing.T) {
	p := writeYAML(t, `
feeds:
  - name: a
    url: https://a.test/rss
`)
	f, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if f.DefaultInterval != time.Hour {
		t.Errorf("default_interval = %s, want 1h", f.DefaultInterval)
	}
}

func TestLoadRejections(t *testing.T) {
	longURL := "https://example.com/" + strings.Repeat("a", MaxURLLength)
	longTitle := strings.Repeat("t", MaxTitleLength+1)

	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			"missing name",
			"feeds:\n  - url: https://a.test/rss\n",
			"name is required",
		},
		{
			"invalid name characters",
			"feeds:\n  - name: BadName\n    url: https://a.test/rss\n",
			`must match [a-z0-9_-]+`,
		},
		{
			"non-http(s) url",
			"feeds:\n  - name: a\n    url: ftp://a.test/rss\n",
			"url must be absolute http(s)",
		},
		{
			"oversized url",
			"feeds:\n  - name: a\n    url: " + longURL + "\n",
			"url is too long",
		},
		{
			"oversized title",
			"feeds:\n  - name: a\n    url: https://a.test/rss\n    title: " + longTitle + "\n",
			"title is too long",
		},
		{
			"duplicate name",
			"feeds:\n  - name: a\n    url: https://a.test/rss\n  - name: a\n    url: https://b.test/rss\n",
			"duplicate name",
		},
		{
			"sub-minimum default interval",
			"default_interval: 10s\nfeeds:\n  - name: a\n    url: https://a.test/rss\n",
			"below minimum",
		},
		{
			"sub-minimum feed interval",
			"feeds:\n  - name: a\n    url: https://a.test/rss\n    interval: 10s\n",
			"below minimum",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeYAML(t, tc.yaml)
			_, err := Load(p)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadOversizedFile(t *testing.T) {
	body := "feeds: []\n" + strings.Repeat("# pad\n", MaxConfigBytes/6+1)
	p := writeYAML(t, body)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "config too large") {
		t.Fatalf("expected 'config too large', got %v", err)
	}
}

func TestFindReturnsFeedOrFalse(t *testing.T) {
	f := &File{Feeds: []Feed{{Name: "alpha", URL: "https://a.test/rss"}}}
	if _, ok := f.Find("alpha"); !ok {
		t.Error("expected Find('alpha') to succeed")
	}
	if _, ok := f.Find("missing"); ok {
		t.Error("expected Find('missing') to fail")
	}
}
