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

	"rss-fulltext/internal/config"
)

type Extractor interface {
	Extract(ctx context.Context, articleURL string) (string, error)
	Sanitize(s string) string
}

// Metrics is the optional metric sink for the generator. Pass nil to disable.
type Metrics interface {
	RecordRefresh(feed, outcome string, duration time.Duration)
	RecordRefreshSuccess(feed string, at time.Time, items int)
}

type Status struct {
	Name          string            `json:"name"`
	Title         string            `json:"title,omitempty"`
	SourceURL     string            `json:"source_url"`
	FileURL       string            `json:"file_url"`
	Formats       map[string]string `json:"formats,omitempty"`
	Interval      string            `json:"interval"`
	LastRefreshAt time.Time         `json:"last_refresh_at,omitempty"`
	LastSuccessAt time.Time         `json:"last_success_at,omitempty"`
	LastRefreshOK bool              `json:"last_refresh_ok"`
	LastError     string            `json:"last_error,omitempty"`
	ItemCount     int               `json:"item_count"`
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
	s, ok := t.m[name]
	if !ok {
		return
	}
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
	MaxStaleness time.Duration
	UserAgent    string
	Tracker      *Tracker
	Metrics      Metrics
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
	if cfg.MaxStaleness <= 0 {
		cfg.MaxStaleness = 24 * time.Hour
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

func (w *Worker) Refresh(parent context.Context) (err error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, w.cfg.FeedTimeout)
	defer cancel()
	defer func() {
		if w.cfg.Metrics == nil {
			return
		}
		outcome := "ok"
		if err != nil {
			outcome = "fail"
		}
		w.cfg.Metrics.RecordRefresh(w.cfg.Feed.Name, outcome, time.Since(start))
	}()

	body, err := w.fetchFeed(ctx)
	if err != nil {
		return w.failRefresh(start, err)
	}

	parser := gofeed.NewParser()
	parsed, err := parser.Parse(strings.NewReader(body))
	if err != nil {
		return w.failRefresh(start, fmt.Errorf("parse: %w", err))
	}

	if w.cfg.MaxItems > 0 && len(parsed.Items) > w.cfg.MaxItems {
		parsed.Items = parsed.Items[:w.cfg.MaxItems]
	}

	out := w.buildFeed(parsed)
	if err := w.enrich(ctx, parsed, out); err != nil {
		return w.failRefresh(start, err)
	}

	if len(out.Items) == 0 {
		return w.handleEmpty(start, out)
	}

	if err := w.writeOutputs(out); err != nil {
		return w.failRefresh(start, fmt.Errorf("write: %w", err))
	}

	w.cfg.Tracker.update(w.cfg.Feed.Name, func(s *Status) {
		s.LastRefreshAt = start
		s.LastSuccessAt = start
		s.LastRefreshOK = true
		s.LastError = ""
		s.ItemCount = len(out.Items)
	})
	if w.cfg.Metrics != nil {
		w.cfg.Metrics.RecordRefreshSuccess(w.cfg.Feed.Name, start, len(out.Items))
	}
	w.cfg.Logger.Info("refreshed",
		"feed", w.cfg.Feed.Name,
		"items", len(out.Items),
		"duration", time.Since(start),
	)
	return nil
}

// handleEmpty applies the staleness policy when a refresh produces zero items.
//
//	no previous file              -> refuse to write, return error
//	previous file within budget   -> keep previous, return error
//	previous file past budget     -> overwrite with empty feed, return error
func (w *Worker) handleEmpty(start time.Time, out *feeds.Feed) error {
	info, statErr := os.Stat(w.OutputFile())
	if statErr != nil {
		return w.failRefresh(start, errors.New("upstream returned 0 items and no previous file exists"))
	}

	age := time.Since(info.ModTime())
	if w.cfg.MaxStaleness > 0 && age > w.cfg.MaxStaleness {
		w.cfg.Logger.Warn("0-items refresh past staleness budget; overwriting with empty feed",
			"feed", w.cfg.Feed.Name,
			"age", age,
			"budget", w.cfg.MaxStaleness)
		if err := w.writeOutputs(out); err != nil {
			return w.failRefresh(start, fmt.Errorf("write empty: %w", err))
		}
		w.cfg.Tracker.update(w.cfg.Feed.Name, func(s *Status) {
			s.LastRefreshAt = start
			s.LastRefreshOK = false
			s.LastError = fmt.Sprintf("upstream returned 0 items for %s (past %s budget)", age.Round(time.Second), w.cfg.MaxStaleness)
			s.ItemCount = 0
		})
		return errors.New("upstream returned 0 items past staleness budget")
	}

	w.cfg.Logger.Warn("upstream returned 0 items; keeping previous output",
		"feed", w.cfg.Feed.Name,
		"age", age)
	return w.failRefresh(start, errors.New("upstream returned 0 items; previous output kept"))
}

func (w *Worker) failRefresh(start time.Time, err error) error {
	w.cfg.Tracker.update(w.cfg.Feed.Name, func(s *Status) {
		s.LastRefreshAt = start
		s.LastRefreshOK = false
		s.LastError = err.Error()
	})
	return err
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
		Description: firstNonEmpty(in.Description, "Full-text feed generated by rss-fulltext"),
		Created:     orNow(in.PublishedParsed),
		Updated:     orNow(in.UpdatedParsed),
	}
	if in.Author != nil {
		f.Author = &feeds.Author{Name: in.Author.Name, Email: in.Author.Email}
	}
	return f
}

func (w *Worker) enrich(ctx context.Context, in *gofeed.Feed, out *feeds.Feed) error {
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
	return ctx.Err()
}

func (w *Worker) itemFor(ctx context.Context, in *gofeed.Item) *feeds.Item {
	if in == nil || strings.TrimSpace(in.Link) == "" {
		return nil
	}
	safeDescription := w.cfg.Extractor.Sanitize(in.Description)
	item := &feeds.Item{
		Title:       firstNonEmpty(in.Title, in.Link),
		Link:        &feeds.Link{Href: in.Link},
		Description: safeDescription,
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
		item.Content = safeDescription
		return item
	}
	item.Content = content
	return item
}

// outputFormats lists every serialization the worker emits per refresh.
// Order matters only for cleanup semantics: all temp files are staged first,
// then renamed in this order, so .xml (the primary, also used for staleness
// checks) is published before its alternates.
var outputFormats = []struct {
	ext   string
	write func(*feeds.Feed, io.Writer) error
}{
	{".xml", func(f *feeds.Feed, w io.Writer) error { return f.WriteRss(w) }},
	{".atom", func(f *feeds.Feed, w io.Writer) error { return f.WriteAtom(w) }},
	{".json", func(f *feeds.Feed, w io.Writer) error { return f.WriteJSON(w) }},
}

// writeOutputs writes every format in outputFormats to disk.
//
// Atomicity policy: all formats are staged (write + fsync + chmod) to dotfile
// temp files in the output dir before any rename happens. A staging failure
// surfaces as an error and leaves the previous published files untouched.
//
// Rename phase: temp files are renamed into place sequentially. Rename within
// a single directory after fsync is effectively infallible on the filesystems
// we run on (ext4, APFS), so partial-rename failure is unreachable in
// practice. If it does happen (e.g. ENOSPC, EROFS race), readers may briefly
// see a mix of fresh and stale formats for one feed — the next refresh
// restores consistency. We accept this rather than carrying backup copies of
// each previous file just to support an unreachable rollback.
func (w *Worker) writeOutputs(out *feeds.Feed) error {
	if err := os.MkdirAll(w.cfg.OutputDir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	type staged struct{ tmp, final string }
	stagedFiles := make([]staged, 0, len(outputFormats))
	cleanup := true
	defer func() {
		if cleanup {
			for _, s := range stagedFiles {
				_ = os.Remove(s.tmp)
			}
		}
	}()

	for _, f := range outputFormats {
		final := filepath.Join(w.cfg.OutputDir, w.cfg.Feed.Name+f.ext)
		name, err := w.stageFile(out, f.ext, f.write)
		if err != nil {
			return err
		}
		stagedFiles = append(stagedFiles, staged{tmp: name, final: final})
	}

	// Past this point the deferred cleanup must not delete files we've already
	// renamed into place. Disable it; per-iteration failures leave only the
	// not-yet-renamed temp files, which we remove inline.
	cleanup = false
	for i, s := range stagedFiles {
		if err := os.Rename(s.tmp, s.final); err != nil {
			for _, leftover := range stagedFiles[i:] {
				_ = os.Remove(leftover.tmp)
			}
			return fmt.Errorf("rename %s: %w", filepath.Base(s.final), err)
		}
	}
	return nil
}

func (w *Worker) stageFile(out *feeds.Feed, ext string, write func(*feeds.Feed, io.Writer) error) (string, error) {
	tmp, err := os.CreateTemp(w.cfg.OutputDir, "."+w.cfg.Feed.Name+ext+".*")
	if err != nil {
		return "", fmt.Errorf("tempfile %s: %w", ext, err)
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(name)
		}
	}()

	bw := bufio.NewWriter(tmp)
	if err := write(out, bw); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write %s: %w", ext, err)
	}
	if err := bw.Flush(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("flush %s: %w", ext, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("sync %s: %w", ext, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close %s: %w", ext, err)
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return "", fmt.Errorf("chmod %s: %w", ext, err)
	}
	ok = true
	return name, nil
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
