package generator

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/feeds"
	"github.com/mmcdole/gofeed"
	"golang.org/x/sync/errgroup"

	"rss-feedgen/internal/config"
)

type Extractor interface {
	Extract(ctx context.Context, articleURL string) (string, error)
}

type Status struct {
	Name          string    `json:"name"`
	Title         string    `json:"title,omitempty"`
	SourceURL     string    `json:"source_url"`
	FileURL       string    `json:"file_url"`
	Interval      string    `json:"interval"`
	LastRefreshAt time.Time `json:"last_refresh_at,omitempty"`
	LastRefreshOK bool      `json:"last_refresh_ok"`
	LastError     string    `json:"last_error,omitempty"`
	ItemCount     int       `json:"item_count"`
}

type Tracker struct {
	mu sync.RWMutex
	m  map[string]Status
}

func NewTracker() *Tracker {
	return &Tracker{m: make(map[string]Status)}
}

func (t *Tracker) Init(name string, s Status) {
	t.mu.Lock()
	t.m[name] = s
	t.mu.Unlock()
}

func (t *Tracker) update(name string, mut func(*Status)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.m[name]
	mut(&s)
	t.m[name] = s
}

func (t *Tracker) Snapshot() []Status {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Status, 0, len(t.m))
	for _, s := range t.m {
		out = append(out, s)
	}
	return out
}

type WorkerConfig struct {
	Feed         config.Feed
	HTTPClient   *http.Client
	Extractor    Extractor
	OutputDir    string
	MaxFeedBytes int64
	MaxItems     int
	Concurrency  int
	FeedTimeout  time.Duration
	UserAgent    string
	Tracker      *Tracker
	Logger       *slog.Logger
}

type Worker struct {
	cfg WorkerConfig
}

func NewWorker(cfg WorkerConfig) *Worker {
	if cfg.MaxFeedBytes <= 0 {
		cfg.MaxFeedBytes = 4 * 1024 * 1024
	}
	if cfg.MaxItems <= 0 {
		cfg.MaxItems = 50
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 4
	}
	if cfg.FeedTimeout <= 0 {
		cfg.FeedTimeout = 90 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Worker{cfg: cfg}
}

func (w *Worker) OutputFile() string {
	return filepath.Join(w.cfg.OutputDir, w.cfg.Feed.Name+".xml")
}

func (w *Worker) Run(ctx context.Context) {
	logger := w.cfg.Logger.With("feed", w.cfg.Feed.Name)

	if err := w.Refresh(ctx); err != nil {
		logger.Warn("initial refresh failed", "err", err)
	}

	t := time.NewTicker(w.cfg.Feed.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.Refresh(ctx); err != nil {
				logger.Warn("refresh failed", "err", err)
			}
		}
	}
}

func (w *Worker) Refresh(parent context.Context) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, w.cfg.FeedTimeout)
	defer cancel()

	body, err := w.fetchFeed(ctx)
	if err != nil {
		w.recordError(start, err)
		return err
	}

	parser := gofeed.NewParser()
	parsed, err := parser.Parse(strings.NewReader(body))
	if err != nil {
		w.recordError(start, fmt.Errorf("parse: %w", err))
		return err
	}

	if w.cfg.MaxItems > 0 && len(parsed.Items) > w.cfg.MaxItems {
		parsed.Items = parsed.Items[:w.cfg.MaxItems]
	}

	out := w.buildFeed(parsed)
	w.enrich(ctx, parsed, out)

	if err := w.writeAtomic(out); err != nil {
		w.recordError(start, fmt.Errorf("write: %w", err))
		return err
	}

	w.cfg.Tracker.update(w.cfg.Feed.Name, func(s *Status) {
		s.LastRefreshAt = start
		s.LastRefreshOK = true
		s.LastError = ""
		s.ItemCount = len(out.Items)
	})
	w.cfg.Logger.Info("refreshed",
		"feed", w.cfg.Feed.Name,
		"items", len(out.Items),
		"duration", time.Since(start),
	)
	return nil
}

func (w *Worker) recordError(start time.Time, err error) {
	w.cfg.Tracker.update(w.cfg.Feed.Name, func(s *Status) {
		s.LastRefreshAt = start
		s.LastRefreshOK = false
		s.LastError = err.Error()
	})
}

func (w *Worker) fetchFeed(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.cfg.Feed.URL, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	if w.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", w.cfg.UserAgent)
	}
	req.Header.Set("Accept", "application/rss+xml,application/atom+xml,application/xml;q=0.9,*/*;q=0.1")

	resp, err := w.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}

	buf, err := io.ReadAll(io.LimitReader(resp.Body, w.cfg.MaxFeedBytes+1))
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	if int64(len(buf)) > w.cfg.MaxFeedBytes {
		return "", fmt.Errorf("feed exceeded %d bytes", w.cfg.MaxFeedBytes)
	}
	return string(buf), nil
}

func (w *Worker) buildFeed(in *gofeed.Feed) *feeds.Feed {
	link := in.Link
	if link == "" {
		link = w.cfg.Feed.URL
	}
	f := &feeds.Feed{
		Title:       firstNonEmpty(w.cfg.Feed.Title, in.Title, "Untitled feed"),
		Link:        &feeds.Link{Href: link},
		Description: firstNonEmpty(in.Description, "Full-text feed generated by rss-feedgen"),
		Created:     orNow(in.PublishedParsed),
		Updated:     orNow(in.UpdatedParsed),
	}
	if in.Author != nil {
		f.Author = &feeds.Author{Name: in.Author.Name, Email: in.Author.Email}
	}
	return f
}

func (w *Worker) enrich(ctx context.Context, in *gofeed.Feed, out *feeds.Feed) {
	items := make([]*feeds.Item, len(in.Items))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(w.cfg.Concurrency)

	for i, it := range in.Items {
		if gctx.Err() != nil {
			break
		}
		g.Go(func() error {
			items[i] = w.itemFor(gctx, it)
			return nil
		})
	}
	_ = g.Wait()

	for _, it := range items {
		if it != nil {
			out.Items = append(out.Items, it)
		}
	}
}

func (w *Worker) itemFor(ctx context.Context, in *gofeed.Item) *feeds.Item {
	if in == nil || strings.TrimSpace(in.Link) == "" {
		return nil
	}
	item := &feeds.Item{
		Title:       firstNonEmpty(in.Title, in.Link),
		Link:        &feeds.Link{Href: in.Link},
		Description: in.Description,
		Created:     orZero(in.PublishedParsed),
		Updated:     orZero(in.UpdatedParsed),
		Id:          firstNonEmpty(in.GUID, in.Link),
	}
	if in.Author != nil {
		item.Author = &feeds.Author{Name: in.Author.Name, Email: in.Author.Email}
	}

	content, err := w.cfg.Extractor.Extract(ctx, in.Link)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			w.cfg.Logger.Warn("extract failed",
				"feed", w.cfg.Feed.Name, "url", in.Link, "err", err)
		}
		item.Content = in.Description
		return item
	}
	item.Content = content
	return item
}

func (w *Worker) writeAtomic(out *feeds.Feed) error {
	if err := os.MkdirAll(w.cfg.OutputDir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	final := w.OutputFile()
	tmp, err := os.CreateTemp(w.cfg.OutputDir, "."+w.cfg.Feed.Name+".xml.*")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	name := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(name)
		}
	}()

	bw := bufio.NewWriter(tmp)
	if err := out.WriteRss(bw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write rss: %w", err)
	}
	if err := bw.Flush(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("flush: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}
	if err := os.Rename(name, final); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	cleanup = false
	return nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func orZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func orNow(t *time.Time) time.Time {
	if t == nil || t.IsZero() {
		return time.Now().UTC()
	}
	return *t
}
