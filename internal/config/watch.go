package config

import (
	"context"
	"log/slog"
	"os"
	"time"
)

// Watch polls path every interval and calls onChange with the freshly loaded
// config whenever the file changes and reloads successfully. It blocks until ctx
// is cancelled.
//
// Change detection uses the file's modification time and size rather than
// inotify/fsnotify: inotify events do not fire reliably across Docker bind
// mounts, whereas os.Stat reflects host-side edits. The file is re-opened by
// path on every poll, so editors that save by writing a temp file and renaming
// it over the original are handled correctly (provided the containing directory,
// not the single file, is mounted).
//
// A reload that fails to parse or validate (for example, a half-written file
// mid-edit) is logged and skipped; the previous config is kept and onChange is
// only invoked on success.
func Watch(ctx context.Context, path string, interval time.Duration, onChange func(*File), logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		return
	}

	var lastMod time.Time
	var lastSize int64
	if info, err := os.Stat(path); err == nil {
		lastMod = info.ModTime()
		lastSize = info.Size()
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			info, err := os.Stat(path)
			if err != nil {
				logger.Warn("config watch: stat failed", "path", path, "err", err)
				continue
			}
			if info.ModTime().Equal(lastMod) && info.Size() == lastSize {
				continue
			}
			// Advance the baseline before attempting to load so a persistently
			// broken file is not re-read every tick; the next real edit changes
			// mtime/size again and triggers a fresh attempt.
			lastMod = info.ModTime()
			lastSize = info.Size()

			f, err := Load(path)
			if err != nil {
				logger.Warn("config watch: reload failed, keeping previous config", "path", path, "err", err)
				continue
			}
			logger.Info("config reloaded", "path", path, "feeds", len(f.Feeds))
			onChange(f)
		}
	}
}
