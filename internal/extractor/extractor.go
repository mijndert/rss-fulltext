package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/microcosm-cc/bluemonday"
)

type Cache interface {
	Get(key string) (string, bool)
	Set(key, value string)
}

// Metrics is the optional metric sink for the extractor. Pass nil to disable.
type Metrics interface {
	RecordExtract(outcome string)
}

var (
	ErrInvalidURL         = errors.New("invalid article url")
	ErrUpstreamStatus     = errors.New("upstream returned non-2xx")
	ErrUnsupportedContent = errors.New("unsupported content-type")
	ErrEmptyContent       = errors.New("readability returned empty content")
)

type Config struct {
	HTTPClient  *http.Client
	UserAgent   string
	MaxBytes    int64
	Cache       Cache
	CacheTTL    time.Duration
	NegativeTTL time.Duration
	Sanitizer   *bluemonday.Policy
	Metrics     Metrics
	Logger      *slog.Logger
}

type Extractor struct {
	client      *http.Client
	userAgent   string
	maxBytes    int64
	cache       Cache
	cacheTTL    time.Duration
	negativeTTL time.Duration
	sanitizer   *bluemonday.Policy
	metrics     Metrics
	logger      *slog.Logger
}

func New(cfg Config) *Extractor {
	if cfg.HTTPClient == nil {
		panic("extractor.New: HTTPClient is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 5 * 1024 * 1024
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 24 * time.Hour
	}
	if cfg.NegativeTTL <= 0 {
		cfg.NegativeTTL = time.Hour
	}
	if cfg.Sanitizer == nil {
		cfg.Sanitizer = defaultPolicy()
	}
	return &Extractor{
		client:      cfg.HTTPClient,
		userAgent:   cfg.UserAgent,
		maxBytes:    cfg.MaxBytes,
		cache:       cfg.Cache,
		cacheTTL:    cfg.CacheTTL,
		negativeTTL: cfg.NegativeTTL,
		sanitizer:   cfg.Sanitizer,
		metrics:     cfg.Metrics,
		logger:      cfg.Logger,
	}
}

func (e *Extractor) recordOutcome(outcome string) {
	if e.metrics != nil {
		e.metrics.RecordExtract(outcome)
	}
}

func (e *Extractor) Sanitize(s string) string {
	return strings.TrimSpace(e.sanitizer.Sanitize(s))
}

type cacheEntry struct {
	Body      string    `json:"b,omitempty"`
	ErrClass  string    `json:"e,omitempty"`
	ExpiresAt time.Time `json:"x"`
}

func (e *Extractor) cacheGet(key string) (cacheEntry, bool) {
	if e.cache == nil {
		return cacheEntry{}, false
	}
	raw, ok := e.cache.Get(key)
	if !ok {
		return cacheEntry{}, false
	}
	var entry cacheEntry
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		return cacheEntry{}, false
	}
	if !entry.ExpiresAt.IsZero() && time.Now().After(entry.ExpiresAt) {
		return cacheEntry{}, false
	}
	return entry, true
}

func (e *Extractor) cacheSet(key string, entry cacheEntry) {
	if e.cache == nil {
		return
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	e.cache.Set(key, string(data))
}

func (e *Extractor) cacheNegative(key, class string) {
	if e.negativeTTL <= 0 {
		return
	}
	e.cacheSet(key, cacheEntry{
		ErrClass:  class,
		ExpiresAt: time.Now().Add(e.negativeTTL),
	})
}

func classToErr(class string) error {
	switch class {
	case "upstream":
		return ErrUpstreamStatus
	case "unsupported":
		return ErrUnsupportedContent
	case "empty":
		return ErrEmptyContent
	default:
		return fmt.Errorf("cached: %s", class)
	}
}

func (e *Extractor) Extract(ctx context.Context, articleURL string) (string, error) {
	// outcome defaults to "error" so any return-path we forget to label still
	// gets counted somewhere. Update it as we classify failures and success.
	outcome := "error"
	defer func() { e.recordOutcome(outcome) }()

	if entry, ok := e.cacheGet(articleURL); ok {
		if entry.ErrClass != "" {
			outcome = "cache_negative"
			return "", classToErr(entry.ErrClass)
		}
		outcome = "cache_hit"
		return entry.Body, nil
	}

	parsed, err := url.Parse(articleURL)
	if err != nil {
		outcome = "invalid_url"
		return "", fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	if !parsed.IsAbs() || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		outcome = "invalid_url"
		return "", fmt.Errorf("%w: scheme must be http(s)", ErrInvalidURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, articleURL, nil)
	if err != nil {
		outcome = "fetch_error"
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", e.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.1")

	resp, err := e.client.Do(req)
	if err != nil {
		outcome = "fetch_error"
		return "", fmt.Errorf("fetch article: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		outcome = "upstream"
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			e.cacheNegative(articleURL, "upstream")
		}
		return "", fmt.Errorf("%w: %d", ErrUpstreamStatus, resp.StatusCode)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		media, _, err := mime.ParseMediaType(ct)
		if err != nil {
			outcome = "unsupported"
			e.cacheNegative(articleURL, "unsupported")
			return "", fmt.Errorf("%w: %q", ErrUnsupportedContent, ct)
		}
		media = strings.ToLower(media)
		if media != "text/html" && media != "application/xhtml+xml" {
			outcome = "unsupported"
			e.cacheNegative(articleURL, "unsupported")
			return "", fmt.Errorf("%w: %s", ErrUnsupportedContent, media)
		}
	}

	article, err := readability.FromReader(io.LimitReader(resp.Body, e.maxBytes), parsed)
	if err != nil {
		outcome = "render_error"
		return "", fmt.Errorf("readability: %w", err)
	}
	if article.Node == nil {
		outcome = "empty"
		e.cacheNegative(articleURL, "empty")
		return "", ErrEmptyContent
	}
	simplifyDOM(article.Node)

	var rendered strings.Builder
	if err := article.RenderHTML(&rendered); err != nil {
		outcome = "render_error"
		return "", fmt.Errorf("readability render: %w", err)
	}

	content := strings.TrimSpace(e.sanitizer.Sanitize(rendered.String()))
	if content == "" {
		outcome = "empty"
		e.cacheNegative(articleURL, "empty")
		return "", ErrEmptyContent
	}

	entry := cacheEntry{Body: content, ExpiresAt: time.Now().Add(e.cacheTTL)}
	e.cacheSet(articleURL, entry)
	if resp.Request != nil && resp.Request.URL != nil {
		if finalURL := resp.Request.URL.String(); finalURL != "" && finalURL != articleURL {
			e.cacheSet(finalURL, entry)
		}
	}
	outcome = "ok"
	return content, nil
}
