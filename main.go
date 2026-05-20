package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"rss-fulltext/internal/config"
	"rss-fulltext/internal/extractor"
	"rss-fulltext/internal/filecache"
	"rss-fulltext/internal/generator"
	"rss-fulltext/internal/metrics"
	"rss-fulltext/internal/safehttp"
	"rss-fulltext/internal/server"
)

// Version metadata populated by goreleaser via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Printf("rss-fulltext %s (commit %s, built %s)\n", version, commit, date)
			return
		case "healthcheck":
			os.Exit(runHealthcheck())
		case "help", "--help", "-h":
			printUsage(os.Stdout)
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", os.Args[1])
			printUsage(os.Stderr)
			os.Exit(2)
		}
	}
	runServer()
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: rss-fulltext [healthcheck|version|help]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  (no args)     run the HTTP server and refresh workers")
	fmt.Fprintln(w, "  healthcheck   probe http://127.0.0.1:<port>/healthz, exit 0 on success")
	fmt.Fprintln(w, "  version       print the version and exit")
	fmt.Fprintln(w, "  help          print this message")
}

// runHealthcheck performs an HTTP GET against the local /healthz endpoint
// and returns 0 on a 2xx response.
func runHealthcheck() int {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	target, err := healthcheckURL(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: bad LISTEN_ADDR %q: %v\n", addr, err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: build request: %v\n", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

// healthcheckURL maps a LISTEN_ADDR value to a probe URL pointing at the
// local interface. Wildcard hosts are rewritten to a valid destination
// (loopback) so the probe still reaches the server when bound to all
// interfaces.
func healthcheckURL(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

func runServer() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	logger.Info("starting", "version", version, "commit", commit)

	cfg, err := loadConfig(logger)
	if err != nil {
		logger.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	if cfg.ConfigPath == "" {
		logger.Error("CONFIG_PATH is required")
		os.Exit(1)
	}

	feeds, err := config.Load(cfg.ConfigPath)
	if err != nil {
		logger.Error("load feeds config", "path", cfg.ConfigPath, "err", err)
		os.Exit(1)
	}
	logger.Info("loaded feeds",
		"path", cfg.ConfigPath,
		"count", len(feeds.Feeds),
		"default_interval", feeds.DefaultInterval)

	cache, err := filecache.New(cfg.CacheDir, cfg.CacheTTL, logger)
	if err != nil {
		logger.Error("init cache", "err", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		logger.Error("create output dir", "dir", cfg.OutputDir, "err", err)
		os.Exit(1)
	}

	client := safehttp.NewClient(cfg.RequestTimeout, safehttp.Options{
		AllowPrivateAddresses: cfg.AllowPrivateAddresses,
	})

	mx := metrics.New()

	ext := extractor.New(extractor.Config{
		HTTPClient:  client,
		UserAgent:   cfg.UserAgent,
		MaxBytes:    cfg.MaxArticleBytes,
		Cache:       cache,
		CacheTTL:    cfg.CacheTTL,
		NegativeTTL: cfg.NegativeCacheTTL,
		Metrics:     mx,
		Logger:      logger,
	})

	tracker := generator.NewTracker()
	for _, f := range feeds.Feeds {
		tracker.Init(f.Name, generator.Status{
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
		})
	}

	srv := server.New(server.Config{
		OutputDir: cfg.OutputDir,
		Tracker:   tracker,
		Metrics:   mx.Handler(),
		Logger:    logger,
	})

	readHeaderTimeout := 10 * time.Second
	if cfg.ReadTimeout > 0 && cfg.ReadTimeout < readHeaderTimeout {
		readHeaderTimeout = cfg.ReadTimeout
	}
	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	if cfg.AllowPrivateAddresses {
		logger.Warn("ALLOW_PRIVATE_ADDRESSES is enabled; SSRF guard is off")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup

	for i, f := range feeds.Feeds {
		w := generator.NewWorker(generator.WorkerConfig{
			Feed:         f,
			HTTPClient:   client,
			Extractor:    ext,
			OutputDir:    cfg.OutputDir,
			MaxFeedBytes: cfg.MaxFeedBytes,
			MaxItems:     cfg.MaxItemsPerFeed,
			Concurrency:  cfg.Concurrency,
			FeedTimeout:  cfg.FeedTimeout,
			MaxStaleness: cfg.MaxStaleness,
			UserAgent:    cfg.UserAgent,
			Tracker:      tracker,
			Metrics:      mx,
			Logger:       logger,
		})
		delay := time.Duration(i) * 500 * time.Millisecond
		wg.Add(1)
		go func(w *generator.Worker, d time.Duration) {
			defer wg.Done()
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return
			}
			w.Run(ctx)
		}(w, delay)
	}

	if cfg.JanitorInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(cfg.JanitorInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					cache.Purge()
				}
			}
		}()
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.ListenAddr, "output_dir", cfg.OutputDir)
		errCh <- httpSrv.ListenAndServe()
	}()

	exitCode := 0
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server terminated", "err", err)
			exitCode = 1
		}
	}

	stop()
	wg.Wait()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
	}

	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

type appConfig struct {
	ListenAddr            string
	ConfigPath            string
	OutputDir             string
	CacheDir              string
	CacheTTL              time.Duration
	JanitorInterval       time.Duration
	Concurrency           int
	RequestTimeout        time.Duration
	ReadTimeout           time.Duration
	WriteTimeout          time.Duration
	FeedTimeout           time.Duration
	MaxArticleBytes       int64
	MaxFeedBytes          int64
	MaxItemsPerFeed       int
	NegativeCacheTTL      time.Duration
	MaxStaleness          time.Duration
	UserAgent             string
	AllowPrivateAddresses bool
}

func loadConfig(logger *slog.Logger) (appConfig, error) {
	c := appConfig{
		ListenAddr:            env("LISTEN_ADDR", "127.0.0.1:8080"),
		ConfigPath:            env("CONFIG_PATH", ""),
		OutputDir:             env("OUTPUT_DIR", "/var/lib/rss-fulltext/feeds"),
		CacheDir:              env("CACHE_DIR", "/var/lib/rss-fulltext/cache"),
		CacheTTL:              envDuration("CACHE_TTL", 24*time.Hour, logger),
		JanitorInterval:       envDuration("JANITOR_INTERVAL", time.Hour, logger),
		Concurrency:           envInt("CONCURRENCY", 4, logger),
		RequestTimeout:        envDuration("REQUEST_TIMEOUT", 20*time.Second, logger),
		ReadTimeout:           envDuration("READ_TIMEOUT", 30*time.Second, logger),
		WriteTimeout:          envDuration("WRITE_TIMEOUT", 30*time.Second, logger),
		FeedTimeout:           envDuration("FEED_TIMEOUT", 5*time.Minute, logger),
		MaxArticleBytes:       envInt64("MAX_ARTICLE_BYTES", 5*1024*1024, logger),
		MaxFeedBytes:          envInt64("MAX_FEED_BYTES", 4*1024*1024, logger),
		MaxItemsPerFeed:       envInt("MAX_ITEMS_PER_FEED", 50, logger),
		NegativeCacheTTL:      envDuration("NEGATIVE_CACHE_TTL", time.Hour, logger),
		MaxStaleness:          envDuration("MAX_STALENESS", 24*time.Hour, logger),
		UserAgent:             env("USER_AGENT", "rss-fulltext/2.0"),
		AllowPrivateAddresses: envBool("ALLOW_PRIVATE_ADDRESSES", false, logger),
	}
	return c, c.validate()
}

func (c appConfig) validate() error {
	if c.Concurrency < 1 {
		return fmt.Errorf("CONCURRENCY must be >= 1, got %d", c.Concurrency)
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("REQUEST_TIMEOUT must be > 0, got %s", c.RequestTimeout)
	}
	if c.FeedTimeout <= 0 {
		return fmt.Errorf("FEED_TIMEOUT must be > 0, got %s", c.FeedTimeout)
	}
	if c.MaxArticleBytes <= 0 {
		return fmt.Errorf("MAX_ARTICLE_BYTES must be > 0, got %d", c.MaxArticleBytes)
	}
	if c.MaxFeedBytes <= 0 {
		return fmt.Errorf("MAX_FEED_BYTES must be > 0, got %d", c.MaxFeedBytes)
	}
	if c.MaxItemsPerFeed < 1 {
		return fmt.Errorf("MAX_ITEMS_PER_FEED must be >= 1, got %d", c.MaxItemsPerFeed)
	}
	if c.OutputDir == "" {
		return errors.New("OUTPUT_DIR must be set")
	}
	if c.CacheDir == "" {
		return errors.New("CACHE_DIR must be set")
	}
	return nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int, logger *slog.Logger) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		logger.Warn("bad int env", "key", key, "value", v, "default", def)
		return def
	}
	return n
}

func envInt64(key string, def int64, logger *slog.Logger) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		logger.Warn("bad int64 env", "key", key, "value", v, "default", def)
		return def
	}
	return n
}

func envBool(key string, def bool, logger *slog.Logger) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		logger.Warn("bad bool env", "key", key, "value", v, "default", def)
		return def
	}
	return b
}

func envDuration(key string, def time.Duration, logger *slog.Logger) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		logger.Warn("bad duration env", "key", key, "value", v, "default", def)
		return def
	}
	return d
}
