package filecache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

type FileCache struct {
	dir    string
	ttl    time.Duration
	logger *slog.Logger
}

func New(dir string, ttl time.Duration, logger *slog.Logger) (*FileCache, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if dir == "" {
		return nil, errors.New("filecache: dir is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("filecache mkdir: %w", err)
	}
	return &FileCache{dir: dir, ttl: ttl, logger: logger}, nil
}

func (c *FileCache) path(key string) string {
	h := sha256.Sum256([]byte(key))
	return filepath.Join(c.dir, hex.EncodeToString(h[:]))
}

func (c *FileCache) Get(key string) (string, bool) {
	p := c.path(key)
	info, err := os.Stat(p)
	if err != nil {
		return "", false
	}
	if c.ttl > 0 && time.Since(info.ModTime()) > c.ttl {
		_ = os.Remove(p)
		return "", false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.logger.Warn("filecache read", "err", err)
		}
		return "", false
	}
	return string(b), true
}

func (c *FileCache) Set(key, value string) {
	if c.ttl <= 0 {
		return
	}
	final := c.path(key)
	tmp, err := os.CreateTemp(c.dir, ".tmp-*")
	if err != nil {
		c.logger.Warn("filecache tempfile", "err", err)
		return
	}
	name := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(name)
		}
	}()
	if _, err := io.WriteString(tmp, value); err != nil {
		_ = tmp.Close()
		c.logger.Warn("filecache write", "err", err)
		return
	}
	if err := tmp.Close(); err != nil {
		c.logger.Warn("filecache close", "err", err)
		return
	}
	if err := os.Chmod(name, 0o600); err != nil {
		c.logger.Warn("filecache chmod", "err", err)
	}
	if err := os.Rename(name, final); err != nil {
		c.logger.Warn("filecache rename", "err", err)
		return
	}
	cleanup = false
}

func (c *FileCache) Purge() {
	if c.ttl <= 0 {
		return
	}
	cutoff := time.Now().Add(-c.ttl)
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		c.logger.Warn("filecache scan", "err", err)
		return
	}
	var removed int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(c.dir, e.Name())); err == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		c.logger.Debug("filecache purged", "count", removed)
	}
}
