package config

import (
	"fmt"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Feed struct {
	Name     string        `yaml:"name"`
	URL      string        `yaml:"url"`
	Title    string        `yaml:"title,omitempty"`
	Interval time.Duration `yaml:"interval,omitempty"`
}

type File struct {
	DefaultInterval time.Duration `yaml:"default_interval,omitempty"`
	Feeds           []Feed        `yaml:"feeds"`
}

const (
	MaxConfigBytes = 1 << 20
	MinInterval    = time.Minute
)

func Load(path string) (*File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat config: %w", err)
	}
	if info.Size() > MaxConfigBytes {
		return nil, fmt.Errorf("config too large: %d bytes (max %d)", info.Size(), MaxConfigBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := f.normalize(); err != nil {
		return nil, err
	}
	return &f, nil
}

func (f *File) normalize() error {
	if f.DefaultInterval == 0 {
		f.DefaultInterval = time.Hour
	}
	if f.DefaultInterval < MinInterval {
		return fmt.Errorf("default_interval %s is below minimum %s", f.DefaultInterval, MinInterval)
	}

	seen := make(map[string]bool, len(f.Feeds))
	for i := range f.Feeds {
		feed := &f.Feeds[i]
		if feed.Name == "" {
			return fmt.Errorf("feed[%d]: name is required", i)
		}
		if !validName(feed.Name) {
			return fmt.Errorf("feed[%d]: name %q must match [a-z0-9_-]+", i, feed.Name)
		}
		if len(feed.Name) > 64 {
			return fmt.Errorf("feed[%d]: name %q is too long (max 64)", i, feed.Name)
		}
		if seen[feed.Name] {
			return fmt.Errorf("feed[%d]: duplicate name %q", i, feed.Name)
		}
		seen[feed.Name] = true
		u, err := url.Parse(feed.URL)
		if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("feed %q: url must be absolute http(s)", feed.Name)
		}
		if feed.Interval == 0 {
			feed.Interval = f.DefaultInterval
		}
		if feed.Interval < MinInterval {
			return fmt.Errorf("feed %q: interval %s is below minimum %s",
				feed.Name, feed.Interval, MinInterval)
		}
	}
	return nil
}

func (f *File) Find(name string) (Feed, bool) {
	for _, feed := range f.Feeds {
		if feed.Name == name {
			return feed, true
		}
	}
	return Feed{}, false
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}
