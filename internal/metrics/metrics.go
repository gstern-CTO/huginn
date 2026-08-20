package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/gstern-CTO/huginn/internal/protocol"
)

// Metrics holds the Prometheus collectors. OctoCode writes a stats file and
// exposes nothing scrapeable; a tool sitting in the critical path of an
// engineering workflow should be a first-class observable service
// (WEAKNESSES.md #10).
type Metrics struct {
	registry *prometheus.Registry

	toolCalls    *prometheus.CounterVec
	toolLatency  *prometheus.HistogramVec
	cacheHits    prometheus.Counter
	cacheMisses  prometheus.Counter
	cacheRatio   prometheus.GaugeFunc
	rateLimit    prometheus.Gauge
	rateWaits    prometheus.Counter
	lspServers   prometheus.Gauge
	redactions   prometheus.Counter
	githubCalls  *prometheus.CounterVec
	hitCount     *atomicCounter
	missCount    *atomicCounter
	enabled      bool
	metricsPort  int
	shutdownFunc func(context.Context) error
}

// NewMetrics builds and registers the collectors against a private registry, so
// the exposed endpoint carries only this server's metrics plus the standard Go
// runtime collectors.
func NewMetrics(enabled bool, port int) *Metrics {
	m := &Metrics{
		registry:    prometheus.NewRegistry(),
		hitCount:    &atomicCounter{},
		missCount:   &atomicCounter{},
		enabled:     enabled,
		metricsPort: port,
	}

	m.toolCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "huginn_tool_calls_total",
		Help: "Total MCP tool invocations by tool name and resulting status.",
	}, []string{"tool", "status"})

	m.toolLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "huginn_tool_latency_seconds",
		Help:    "MCP tool call latency in seconds by tool name.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
	}, []string{"tool"})

	m.cacheHits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "huginn_cache_hits_total",
		Help: "Cache lookups served from the memory or disk tier.",
	})
	m.cacheMisses = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "huginn_cache_misses_total",
		Help: "Cache lookups that missed both tiers.",
	})
	m.cacheRatio = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "huginn_cache_hit_ratio",
		Help: "Cache hit ratio since process start (0-1).",
	}, func() float64 {
		hits, misses := m.hitCount.load(), m.missCount.load()
		total := hits + misses
		if total == 0 {
			return 0
		}
		return float64(hits) / float64(total)
	})

	m.rateLimit = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "huginn_github_rate_limit_remaining",
		Help: "Requests remaining in the current GitHub rate limit window.",
	})
	m.rateWaits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "huginn_github_rate_limit_waits_total",
		Help: "Times a request slept until the GitHub rate limit reset.",
	})
	m.lspServers = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "huginn_lsp_servers_active",
		Help: "Language server processes currently held open.",
	})
	m.redactions = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "huginn_secrets_redacted_total",
		Help: "Secret-shaped strings replaced before leaving the server.",
	})
	m.githubCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "huginn_github_requests_total",
		Help: "GitHub API requests by outcome.",
	}, []string{"outcome"})

	m.registry.MustRegister(
		m.toolCalls, m.toolLatency, m.cacheHits, m.cacheMisses, m.cacheRatio,
		m.rateLimit, m.rateWaits, m.lspServers, m.redactions, m.githubCalls,
	)
	return m
}

// Serve starts the metrics endpoint on its own port. It never writes to stdout:
// stdout belongs exclusively to the MCP protocol.
func (m *Metrics) Serve(logger *slog.Logger) {
	if m == nil || !m.enabled {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", m.metricsPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	m.shutdownFunc = srv.Shutdown

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// A busy metrics port must not take the MCP server down.
			logger.Warn("metrics endpoint unavailable", "addr", srv.Addr, "error", err)
		}
	}()
	logger.Info("metrics endpoint listening", "addr", srv.Addr+"/metrics")
}

// Shutdown stops the metrics endpoint if it is running.
func (m *Metrics) Shutdown(ctx context.Context) {
	if m == nil || m.shutdownFunc == nil {
		return
	}
	_ = m.shutdownFunc(ctx)
}

func (m *Metrics) RecordToolCall(tool string, status protocol.Status, d time.Duration) {
	if m == nil {
		return
	}
	m.toolCalls.WithLabelValues(tool, string(status)).Inc()
	m.toolLatency.WithLabelValues(tool).Observe(d.Seconds())
}

func (m *Metrics) RecordCacheHit() {
	if m == nil {
		return
	}
	m.cacheHits.Inc()
	m.hitCount.add(1)
}

func (m *Metrics) RecordCacheMiss() {
	if m == nil {
		return
	}
	m.cacheMisses.Inc()
	m.missCount.add(1)
}

func (m *Metrics) SetRateLimitRemaining(n int) {
	if m == nil {
		return
	}
	m.rateLimit.Set(float64(n))
}

func (m *Metrics) RecordRateLimitWait() {
	if m == nil {
		return
	}
	m.rateWaits.Inc()
}

func (m *Metrics) SetActiveLSPServers(n int) {
	if m == nil {
		return
	}
	m.lspServers.Set(float64(n))
}

func (m *Metrics) RecordRedactions(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.redactions.Add(float64(n))
}

func (m *Metrics) RecordGitHubRequest(outcome string) {
	if m == nil {
		return
	}
	m.githubCalls.WithLabelValues(outcome).Inc()
}

// atomicCounter is a minimal monotonic counter backing the hit-ratio gauge.
type atomicCounter struct{ v atomic.Int64 }

func (c *atomicCounter) add(n int64) { c.v.Add(n) }
func (c *atomicCounter) load() int64 { return c.v.Load() }

// Hits and Misses expose the raw counts for tests and diagnostics.
func (m *Metrics) Hits() int64   { return m.hitCount.load() }
func (m *Metrics) Misses() int64 { return m.missCount.load() }
