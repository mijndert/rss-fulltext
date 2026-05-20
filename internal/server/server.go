package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"rss-fulltext/internal/generator"
)

type Config struct {
	OutputDir string
	Tracker   *generator.Tracker
	// Metrics is the /metrics handler. Nil disables the endpoint.
	Metrics http.Handler
	Logger  *slog.Logger
}

type Server struct {
	outputDir string
	tracker   *generator.Tracker
	fileSrv   http.Handler
	metrics   http.Handler
	logger    *slog.Logger
}

func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Server{
		outputDir: cfg.OutputDir,
		tracker:   cfg.Tracker,
		fileSrv:   http.FileServer(safeDir(cfg.OutputDir)),
		metrics:   cfg.Metrics,
		logger:    cfg.Logger,
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /feeds.json", s.handleListFeeds)
	if s.metrics != nil {
		mux.Handle("GET /metrics", s.metrics)
	}
	mux.HandleFunc("GET /", s.handleStatic)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleListFeeds(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"feeds": s.tracker.Snapshot()})
}

// feedContentTypes maps the served file extension to its canonical media type.
// Extensions not in this map are rejected with 404.
var feedContentTypes = map[string]string{
	".xml":  "application/rss+xml; charset=utf-8",
	".atom": "application/atom+xml; charset=utf-8",
	".json": "application/feed+json; charset=utf-8",
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/" {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(path, "/")
	if strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	ext := filepath.Ext(name)
	ct, ok := feedContentTypes[ext]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.fileSrv.ServeHTTP(w, r)
}

func safeDir(root string) http.FileSystem {
	return restrictedDir{root: filepath.Clean(root)}
}

type restrictedDir struct {
	root string
}

func (d restrictedDir) Open(name string) (http.File, error) {
	clean := filepath.Clean("/" + name)
	if strings.Contains(clean, "..") {
		return nil, http.ErrNotSupported
	}
	return http.Dir(d.root).Open(clean)
}
