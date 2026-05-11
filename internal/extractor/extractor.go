package extractor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-shiori/go-readability"
	"github.com/microcosm-cc/bluemonday"
)

type Cache interface {
	Get(key string) (string, bool)
	Set(key, value string)
}

var (
	ErrInvalidURL         = errors.New("invalid article url")
	ErrUpstreamStatus     = errors.New("upstream returned non-2xx")
	ErrUnsupportedContent = errors.New("unsupported content-type")
	ErrEmptyContent       = errors.New("readability returned empty content")
)

type Config struct {
	HTTPClient *http.Client
	UserAgent  string
	MaxBytes   int64
	Cache      Cache
	Sanitizer  *bluemonday.Policy
	Logger     *slog.Logger
}

type Extractor struct {
	client    *http.Client
	userAgent string
	maxBytes  int64
	cache     Cache
	sanitizer *bluemonday.Policy
	logger    *slog.Logger
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
	if cfg.Sanitizer == nil {
		cfg.Sanitizer = bluemonday.UGCPolicy()
	}
	return &Extractor{
		client:    cfg.HTTPClient,
		userAgent: cfg.UserAgent,
		maxBytes:  cfg.MaxBytes,
		cache:     cfg.Cache,
		sanitizer: cfg.Sanitizer,
		logger:    cfg.Logger,
	}
}

func (e *Extractor) Extract(ctx context.Context, articleURL string) (string, error) {
	if e.cache != nil {
		if v, ok := e.cache.Get(articleURL); ok {
			return v, nil
		}
	}

	parsed, err := url.Parse(articleURL)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	if !parsed.IsAbs() || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("%w: scheme must be http(s)", ErrInvalidURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, articleURL, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", e.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.1")

	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch article: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%w: %d", ErrUpstreamStatus, resp.StatusCode)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		media, _, err := mime.ParseMediaType(ct)
		if err != nil {
			return "", fmt.Errorf("%w: %q", ErrUnsupportedContent, ct)
		}
		media = strings.ToLower(media)
		if media != "text/html" && media != "application/xhtml+xml" {
			return "", fmt.Errorf("%w: %s", ErrUnsupportedContent, media)
		}
	}

	article, err := readability.FromReader(io.LimitReader(resp.Body, e.maxBytes), parsed)
	if err != nil {
		return "", fmt.Errorf("readability: %w", err)
	}

	content := strings.TrimSpace(article.Content)
	if content == "" {
		return "", ErrEmptyContent
	}

	content = strings.TrimSpace(e.sanitizer.Sanitize(content))
	if content == "" {
		return "", ErrEmptyContent
	}

	if e.cache != nil {
		e.cache.Set(articleURL, content)
	}
	return content, nil
}
