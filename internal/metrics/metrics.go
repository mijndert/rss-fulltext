// Package metrics provides a tiny in-process Prometheus exposition.
//
// It deliberately avoids the prometheus/client_golang dependency: the
// metric set is small and the text format is stable. Counters and gauges
// are the only supported metric kinds.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const contentType = "text/plain; version=0.0.4; charset=utf-8"

type kind int

const (
	kindCounter kind = iota
	kindGauge
)

func (k kind) String() string {
	if k == kindCounter {
		return "counter"
	}
	return "gauge"
}

type metric struct {
	name   string
	help   string
	kind   kind
	labels []string

	mu     sync.Mutex
	values map[string]float64 // label-value key -> value
}

// Registry holds metric definitions and their values.
type Registry struct {
	mu      sync.RWMutex
	metrics map[string]*metric
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{metrics: make(map[string]*metric)}
}

// Handler returns an http.Handler that serves the Prometheus text exposition.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		_ = r.WriteText(w)
	})
}

// WriteText writes the current snapshot in Prometheus text format.
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.RLock()
	names := make([]string, 0, len(r.metrics))
	for n := range r.metrics {
		names = append(names, n)
	}
	r.mu.RUnlock()
	sort.Strings(names)

	for _, n := range names {
		r.mu.RLock()
		m := r.metrics[n]
		r.mu.RUnlock()
		if err := m.writeText(w); err != nil {
			return err
		}
	}
	return nil
}

func (r *Registry) register(name, help string, k kind, labels []string) *metric {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.metrics[name]; ok {
		// Caller asked for the same metric twice; return the existing
		// definition so the resulting Counter/Gauge wrapper still points
		// at the live value map. Mismatched re-registration is a bug.
		if m.kind != k || !equalLabels(m.labels, labels) {
			panic(fmt.Sprintf("metrics: %s re-registered with different shape", name))
		}
		return m
	}
	m := &metric{
		name:   name,
		help:   help,
		kind:   k,
		labels: append([]string(nil), labels...),
		values: make(map[string]float64),
	}
	r.metrics[name] = m
	return m
}

func (m *metric) writeText(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "# HELP %s %s\n", m.name, escapeHelp(m.help)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "# TYPE %s %s\n", m.name, m.kind); err != nil {
		return err
	}

	m.mu.Lock()
	keys := make([]string, 0, len(m.values))
	for k := range m.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := m.values[k]
		if len(m.labels) == 0 {
			if _, err := fmt.Fprintf(w, "%s %s\n", m.name, formatFloat(v)); err != nil {
				m.mu.Unlock()
				return err
			}
			continue
		}
		parts := strings.Split(k, "\x00")
		var sb strings.Builder
		sb.WriteString(m.name)
		sb.WriteByte('{')
		for i, lbl := range m.labels {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(lbl)
			sb.WriteString(`="`)
			sb.WriteString(escapeLabelValue(parts[i]))
			sb.WriteByte('"')
		}
		sb.WriteByte('}')
		sb.WriteByte(' ')
		sb.WriteString(formatFloat(v))
		sb.WriteByte('\n')
		if _, err := io.WriteString(w, sb.String()); err != nil {
			m.mu.Unlock()
			return err
		}
	}
	m.mu.Unlock()
	return nil
}

func (m *metric) labelKey(values []string) string {
	if len(values) != len(m.labels) {
		panic(fmt.Sprintf("metrics: %s expects %d label values, got %d", m.name, len(m.labels), len(values)))
	}
	return strings.Join(values, "\x00")
}

// Counter is a monotonically increasing value.
type Counter struct{ m *metric }

// NewCounter registers (or retrieves) a counter with the given labels.
func (r *Registry) NewCounter(name, help string, labels ...string) *Counter {
	return &Counter{m: r.register(name, help, kindCounter, labels)}
}

// Inc increments by 1.
func (c *Counter) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Add adds delta. Negative deltas panic — counters are monotonic.
func (c *Counter) Add(delta float64, labelValues ...string) {
	if delta < 0 {
		panic(fmt.Sprintf("metrics: counter %s decremented by %v", c.m.name, delta))
	}
	key := c.m.labelKey(labelValues)
	c.m.mu.Lock()
	c.m.values[key] += delta
	c.m.mu.Unlock()
}

// Gauge is an arbitrary instantaneous value.
type Gauge struct{ m *metric }

// NewGauge registers (or retrieves) a gauge with the given labels.
func (r *Registry) NewGauge(name, help string, labels ...string) *Gauge {
	return &Gauge{m: r.register(name, help, kindGauge, labels)}
}

// Set sets the gauge to v.
func (g *Gauge) Set(v float64, labelValues ...string) {
	key := g.m.labelKey(labelValues)
	g.m.mu.Lock()
	g.m.values[key] = v
	g.m.mu.Unlock()
}

// Metrics is the concrete set of metrics this binary publishes.
type Metrics struct {
	registry        *Registry
	refreshTotal    *Counter
	refreshDuration *Gauge
	refreshItems    *Gauge
	refreshLastOK   *Gauge
	extractTotal    *Counter
}

// New returns a fully-registered Metrics bound to a fresh registry.
func New() *Metrics {
	r := NewRegistry()
	return &Metrics{
		registry: r,
		refreshTotal: r.NewCounter(
			"rss_fulltext_refresh_total",
			"Number of feed refresh attempts by outcome (ok|fail).",
			"feed", "outcome",
		),
		refreshDuration: r.NewGauge(
			"rss_fulltext_refresh_duration_seconds",
			"Duration of the most recent refresh attempt, in seconds.",
			"feed",
		),
		refreshItems: r.NewGauge(
			"rss_fulltext_refresh_items",
			"Item count from the most recent successful refresh.",
			"feed",
		),
		refreshLastOK: r.NewGauge(
			"rss_fulltext_refresh_last_success_timestamp_seconds",
			"Unix timestamp (seconds) of the most recent successful refresh.",
			"feed",
		),
		extractTotal: r.NewCounter(
			"rss_fulltext_extract_total",
			"Article extraction outcomes (ok|cache_hit|cache_negative|upstream|unsupported|empty|invalid_url|fetch_error|render_error|error).",
			"outcome",
		),
	}
}

// Handler returns the /metrics handler bound to this metric set.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.NotFound(w, &http.Request{})
		})
	}
	return m.registry.Handler()
}

// RecordRefresh observes a refresh attempt and its duration.
// outcome should be "ok" or "fail".
func (m *Metrics) RecordRefresh(feed, outcome string, duration time.Duration) {
	if m == nil {
		return
	}
	m.refreshTotal.Inc(feed, outcome)
	m.refreshDuration.Set(duration.Seconds(), feed)
}

// RecordRefreshSuccess records the item count and last-success timestamp
// for a successful refresh. Only call this on success.
func (m *Metrics) RecordRefreshSuccess(feed string, at time.Time, items int) {
	if m == nil {
		return
	}
	m.refreshItems.Set(float64(items), feed)
	m.refreshLastOK.Set(float64(at.Unix()), feed)
}

// RecordExtract observes the outcome of a single article extraction.
func (m *Metrics) RecordExtract(outcome string) {
	if m == nil {
		return
	}
	m.extractTotal.Inc(outcome)
}

func equalLabels(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// escapeLabelValue escapes per the Prometheus text format: \, ", and \n.
func escapeLabelValue(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s) + 4)
	for _, r := range s {
		switch r {
		case '\\':
			sb.WriteString(`\\`)
		case '"':
			sb.WriteString(`\"`)
		case '\n':
			sb.WriteString(`\n`)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// escapeHelp escapes per the Prometheus text format: \ and \n (no quote escaping).
func escapeHelp(s string) string {
	if !strings.ContainsAny(s, "\\\n") {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s) + 4)
	for _, r := range s {
		switch r {
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func formatFloat(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
