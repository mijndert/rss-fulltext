package generator

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"rss-fulltext/internal/config"
)

// startStagger spaces out the initial batch of workers so a large feed list does
// not fire every outbound fetch at once on startup. Feeds added later (via a
// config reload) start immediately.
const startStagger = 500 * time.Millisecond

// ManagerConfig holds the dependencies every worker the Manager runs shares.
// It mirrors the per-worker fields of WorkerConfig minus the Feed itself, which
// the Manager supplies per feed.
type ManagerConfig struct {
	HTTPClient   *http.Client
	Extractor    Extractor
	OutputDir    string
	MaxFeedBytes int64
	MaxItems     int
	Concurrency  int
	FeedTimeout  time.Duration
	MaxStaleness time.Duration
	UserAgent    string
	Tracker      *Tracker
	Metrics      Metrics
	Logger       *slog.Logger
}

type handle struct {
	feed   config.Feed
	cancel context.CancelFunc
	done   chan struct{}
}

// Manager owns the set of running feed workers and reconciles it against a
// desired feed list. Apply can be called repeatedly (e.g. after a config
// reload) to add, remove, or restart workers without recreating the process.
type Manager struct {
	cfg     ManagerConfig
	mu      sync.Mutex
	workers map[string]*handle
	started bool
}

func NewManager(cfg ManagerConfig) *Manager {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Manager{cfg: cfg, workers: make(map[string]*handle)}
}

// Apply reconciles the running workers against feeds. New feeds are started,
// removed feeds are stopped (and their output files deleted), and feeds whose
// URL, interval, or title changed are restarted so the change takes effect and a
// fresh refresh runs. Applying the same feed set twice is a no-op.
//
// New workers are children of ctx; cancelling it (or calling Shutdown) stops
// them all. Apply is safe to call concurrently but reconciliation is serialized.
func (m *Manager) Apply(ctx context.Context, feeds []config.Feed) {
	m.mu.Lock()
	defer m.mu.Unlock()

	desired := make(map[string]config.Feed, len(feeds))
	for _, f := range feeds {
		desired[f.Name] = f
	}

	// Stop workers that are gone or whose definition changed. A changed feed is
	// removed here without deleting its files, then re-added below.
	for name, h := range m.workers {
		want, ok := desired[name]
		if !ok {
			m.stopLocked(name, h)
			m.cfg.Tracker.Remove(name)
			m.removeOutputs(name)
			continue
		}
		if feedChanged(h.feed, want) {
			m.stopLocked(name, h)
		}
	}

	// Start feeds that are not currently running (new or just restarted).
	initial := !m.started
	newCount := 0
	for _, f := range feeds {
		if _, running := m.workers[f.Name]; running {
			continue
		}
		var delay time.Duration
		if initial {
			delay = time.Duration(newCount) * startStagger
		}
		newCount++
		m.startLocked(ctx, f, delay)
	}
	m.started = true
}

// Shutdown stops every running worker and waits for them to drain.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, h := range m.workers {
		m.stopLocked(name, h)
	}
}

// Names returns the names of the currently running workers, unsorted.
func (m *Manager) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.workers))
	for name := range m.workers {
		out = append(out, name)
	}
	return out
}

// startLocked builds and launches a worker for f. Caller must hold m.mu.
func (m *Manager) startLocked(parent context.Context, f config.Feed, delay time.Duration) {
	m.cfg.Tracker.Init(f.Name, statusFor(f))

	wctx, cancel := context.WithCancel(parent)
	w := NewWorker(WorkerConfig{
		Feed:         f,
		HTTPClient:   m.cfg.HTTPClient,
		Extractor:    m.cfg.Extractor,
		OutputDir:    m.cfg.OutputDir,
		MaxFeedBytes: m.cfg.MaxFeedBytes,
		MaxItems:     m.cfg.MaxItems,
		Concurrency:  m.cfg.Concurrency,
		FeedTimeout:  m.cfg.FeedTimeout,
		MaxStaleness: m.cfg.MaxStaleness,
		UserAgent:    m.cfg.UserAgent,
		Tracker:      m.cfg.Tracker,
		Metrics:      m.cfg.Metrics,
		Logger:       m.cfg.Logger,
	})

	done := make(chan struct{})
	m.workers[f.Name] = &handle{feed: f, cancel: cancel, done: done}

	go func() {
		defer close(done)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-wctx.Done():
				return
			}
		}
		w.Run(wctx)
	}()
}

// stopLocked cancels a worker and waits for it to exit, then drops it from the
// map. Waiting matters: it prevents a draining worker from writing output files
// after a removal, or two workers for the same name racing during a restart.
// Caller must hold m.mu.
func (m *Manager) stopLocked(name string, h *handle) {
	h.cancel()
	<-h.done
	delete(m.workers, name)
}

// removeOutputs deletes every generated file for a feed that has been removed.
func (m *Manager) removeOutputs(name string) {
	for _, f := range outputFormats {
		path := filepath.Join(m.cfg.OutputDir, name+f.ext)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			m.cfg.Logger.Warn("remove output file", "feed", name, "file", path, "err", err)
		}
	}
}

// feedChanged reports whether the parts of a feed that affect its worker or
// output differ between two definitions with the same name.
func feedChanged(a, b config.Feed) bool {
	return a.URL != b.URL || a.Interval != b.Interval || a.Title != b.Title
}

func statusFor(f config.Feed) Status {
	return Status{
		Name:      f.Name,
		Title:     f.Title,
		SourceURL: f.URL,
		FileURL:   "/" + f.Name + ".xml",
		Formats: map[string]string{
			"rss":  "/" + f.Name + ".xml",
			"atom": "/" + f.Name + ".atom",
			"json": "/" + f.Name + ".json",
		},
		Interval: f.Interval.String(),
	}
}
