package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"rss-feedgen/internal/config"
	"rss-feedgen/internal/extractor"
	"rss-feedgen/internal/filecache"
	"rss-feedgen/internal/generator"
	"rss-feedgen/internal/safehttp"
	"rss-feedgen/internal/server"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

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

	ext := extractor.New(extractor.Config{
		HTTPClient: client,
		UserAgent:  cfg.UserAgent,
		MaxBytes:   cfg.MaxArticleBytes,
		Cache:      cache,
		Logger:     logger,
	})

	tracker := generator.NewTracker()
	for _, f := range feeds.Feeds {
		tracker.Init(f.Name, generator.Status{
			Name:      f.Name,
			Title:     f.Title,
			SourceURL: f.URL,
			FileURL:   "/" + f.Name + ".xml",
			Interval:  f.Interval.String(),
		})
	}

	srv := server.New(server.Config{
		OutputDir: cfg.OutputDir,
		Tracker:   tracker,
		Logger:    logger,
	})

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
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
			UserAgent:    cfg.UserAgent,
			Tracker:      tracker,
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

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server terminated", "err", err)
			stop()
			wg.Wait()
			os.Exit(1)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
	}
	stop()
	wg.Wait()
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
	UserAgent             string
	AllowPrivateAddresses bool
}

func loadConfig(logger *slog.Logger) (appConfig, error) {
	c := appConfig{
		ListenAddr:            env("LISTEN_ADDR", "127.0.0.1:8080"),
		ConfigPath:            env("CONFIG_PATH", ""),
		OutputDir:             env("OUTPUT_DIR", "/var/lib/rss-feedgen/feeds"),
		CacheDir:              env("CACHE_DIR", "/var/lib/rss-feedgen/cache"),
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
		UserAgent:             env("USER_AGENT", "rss-feedgen/2.0"),
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
