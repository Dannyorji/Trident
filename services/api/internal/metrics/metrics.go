// Package metrics provides a client_golang-backed Prometheus registry served
// on its own internal port (issue #58), separate from the public API port.
//
// It is additive to the pre-existing hand-rolled, dependency-free
// Prometheus-text metrics mounted at GET /metrics on the public mux
// (handlers.MetricsHandler and friends) — that endpoint is untouched. This
// package covers the specific gaps called out by #58: per-endpoint HTTP
// request counts/latency, active WebSocket connections and message totals,
// outbound gRPC call metrics, and rate-limiting rejections.
package metrics

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// DefaultPort is used when METRICS_PORT is unset.
const DefaultPort = "9091"

// Registry is a dedicated registry (not the global default) so this endpoint
// exposes exactly the collectors defined here — no Go-runtime default
// collectors mixed in.
var Registry = prometheus.NewRegistry()

var (
	HTTPRequestsTotal = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "trident_http_requests_total",
		Help: "Total HTTP requests handled by the Go API, by method, route pattern, and status code.",
	}, []string{"method", "path", "status"})

	HTTPRequestDuration = promauto.With(Registry).NewHistogramVec(prometheus.HistogramOpts{
		Name:    "trident_http_request_duration_seconds",
		Help:    "HTTP request duration in seconds, by method, route pattern, and status code.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path", "status"})

	WSActiveConnections = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "trident_ws_active_connections",
		Help: "Currently active WebSocket subscribers (REST WS + GraphQL subscriptions).",
	})

	WSConnectsTotal = promauto.With(Registry).NewCounter(prometheus.CounterOpts{
		Name: "trident_ws_connects_total",
		Help: "Total WebSocket subscriber registrations since startup.",
	})

	// Webhook delivery observability (issue #454). The per-subscription and
	// global concurrency caps only help if an operator can see them binding —
	// without these, a saturated delivery pool and a healthy one look
	// identical from outside.
	WebhookDeliveriesTotal = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "trident_webhook_deliveries_total",
		Help: "Webhook delivery attempts by outcome.",
	}, []string{"outcome"}) // outcome: success|failure|skipped_in_flight|blocked_url

	WebhookDeliveriesInFlight = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "trident_webhook_deliveries_in_flight",
		Help: "Webhook deliveries currently executing, bounded by the global delivery semaphore.",
	})

	WSDisconnectsTotal = promauto.With(Registry).NewCounter(prometheus.CounterOpts{
		Name: "trident_ws_disconnects_total",
		Help: "Total WebSocket subscriber unregistrations since startup.",
	})

	WSMessagesTotal = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "trident_ws_messages_total",
		Help: "Total WebSocket broadcast messages, by outcome.",
	}, []string{"result"}) // result: sent|dropped

	GRPCClientRequestsTotal = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "trident_grpc_client_requests_total",
		Help: "Total outbound gRPC client call attempts, by method and status code.",
	}, []string{"method", "code"})

	GRPCClientRequestDuration = promauto.With(Registry).NewHistogramVec(prometheus.HistogramOpts{
		Name:    "trident_grpc_client_request_duration_seconds",
		Help:    "Outbound gRPC client call duration in seconds, by method and status code.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "code"})

	RateLimitRejectionsTotal = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "trident_ratelimit_rejections_total",
		Help: "Total requests rejected by a rate limiter, by limiter.",
	}, []string{"limiter"}) // limiter: per_key|per_ip|global_concurrency

	RateLimitFailOpenTotal = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "trident_ratelimit_fail_open_total",
		Help: "Total requests allowed because a rate-limit backend check failed, by limiter.",
	}, []string{"limiter"}) // limiter: per_key

	// PanicsRecoveredTotal counts handler-chain panics caught by
	// middleware.Recover (issue #610) — otherwise an invisible failure mode:
	// the client sees a dropped connection, nothing is logged, nothing is
	// counted.
	PanicsRecoveredTotal = promauto.With(Registry).NewCounter(prometheus.CounterOpts{
		Name: "trident_panics_recovered_total",
		Help: "Total handler-chain panics caught by the recovery middleware.",
	})

	// DB pool saturation metrics (issue #238), sourced from pgxpool.Pool.Stat()
	// by PollDBPool. All exposed as Gauges — Stat() itself only returns
	// point-in-time cumulative totals (not deltas), which Set() reflects
	// directly; Prometheus rate()/increase() work the same over a
	// monotonically-increasing Gauge as over a Counter.
	DBPoolMaxConns = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "trident_db_pool_max_conns",
		Help: "Configured maximum size of the Postgres connection pool.",
	})
	DBPoolTotalConns = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "trident_db_pool_total_conns",
		Help: "Current total connections in the Postgres pool (idle + in-use + being established).",
	})
	DBPoolAcquiredConns = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "trident_db_pool_acquired_conns",
		Help: "Connections currently acquired (in use) from the Postgres pool.",
	})
	DBPoolIdleConns = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "trident_db_pool_idle_conns",
		Help: "Idle connections currently available in the Postgres pool.",
	})
	DBPoolConstructingConns = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "trident_db_pool_constructing_conns",
		Help: "Connections currently being established for the Postgres pool.",
	})
	DBPoolAcquireCount = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "trident_db_pool_acquire_count",
		Help: "Cumulative number of successful connection acquisitions from the Postgres pool.",
	})
	DBPoolEmptyAcquireCount = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "trident_db_pool_empty_acquire_count",
		Help: "Cumulative number of acquisitions that had to wait because the Postgres pool had no idle connection — a direct saturation signal.",
	})
	DBPoolCanceledAcquireCount = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "trident_db_pool_canceled_acquire_count",
		Help: "Cumulative number of connection acquisitions canceled before completion (e.g. caller's context expired while waiting).",
	})
	DBPoolAcquireDurationSeconds = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "trident_db_pool_acquire_duration_seconds",
		Help: "Cumulative time spent acquiring connections from the Postgres pool, in seconds.",
	})
	DBPoolEmptyAcquireWaitSeconds = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "trident_db_pool_empty_acquire_wait_seconds",
		Help: "Cumulative time acquisitions spent waiting for a connection because the Postgres pool was empty, in seconds — a direct saturation signal.",
	})
	DBPoolNewConnsCount = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "trident_db_pool_new_conns_count",
		Help: "Cumulative number of new connections established for the Postgres pool.",
	})
	DBPoolMaxIdleDestroyCount = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "trident_db_pool_max_idle_destroy_count",
		Help: "Cumulative number of connections destroyed for exceeding MaxConnIdleTime.",
	})
	DBPoolMaxLifetimeDestroyCount = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "trident_db_pool_max_lifetime_destroy_count",
		Help: "Cumulative number of connections destroyed for exceeding MaxConnLifetime.",
	})

	// RetentionRowsDeletedTotal makes the periodic retention job's effect
	// observable (issue #604): without it, a disk-growth alert has no
	// corresponding "and here is what the remediation actually did" signal,
	// so an operator cannot tell a healthy pruning job from a silently
	// broken one just by watching disk usage trend flat.
	RetentionRowsDeletedTotal = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "trident_retention_rows_deleted_total",
		Help: "Rows deleted by the periodic retention job, by table.",
	}, []string{"table"})
)

// PollDBPool periodically snapshots pool.Stat() into the DB pool gauges
// above (issue #238) until ctx is done. Runs once immediately so the gauges
// are populated before the first tick.
func PollDBPool(ctx context.Context, pool *pgxpool.Pool, interval time.Duration) {
	report := func() {
		stat := pool.Stat()
		DBPoolMaxConns.Set(float64(stat.MaxConns()))
		DBPoolTotalConns.Set(float64(stat.TotalConns()))
		DBPoolAcquiredConns.Set(float64(stat.AcquiredConns()))
		DBPoolIdleConns.Set(float64(stat.IdleConns()))
		DBPoolConstructingConns.Set(float64(stat.ConstructingConns()))
		DBPoolAcquireCount.Set(float64(stat.AcquireCount()))
		DBPoolEmptyAcquireCount.Set(float64(stat.EmptyAcquireCount()))
		DBPoolCanceledAcquireCount.Set(float64(stat.CanceledAcquireCount()))
		DBPoolAcquireDurationSeconds.Set(stat.AcquireDuration().Seconds())
		DBPoolEmptyAcquireWaitSeconds.Set(stat.EmptyAcquireWaitTime().Seconds())
		DBPoolNewConnsCount.Set(float64(stat.NewConnsCount()))
		DBPoolMaxIdleDestroyCount.Set(float64(stat.MaxIdleDestroyCount()))
		DBPoolMaxLifetimeDestroyCount.Set(float64(stat.MaxLifetimeDestroyCount()))
	}

	report()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			report()
		}
	}
}

// Port returns the port the metrics server listens on (METRICS_PORT, or
// DefaultPort).
func Port() string {
	if p := os.Getenv("METRICS_PORT"); p != "" {
		return p
	}
	return DefaultPort
}

// Handler builds a mux exposing only GET /metrics, backed by Registry.
// Exposed for testing.
func Handler() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(Registry, promhttp.HandlerOpts{}))
	return mux
}

// Start launches the internal metrics server on METRICS_PORT (default 9091)
// and returns its *http.Server so the caller can shut it down.
func Start() *http.Server {
	addr := ":" + Port()
	srv := &http.Server{
		Addr:              addr,
		Handler:           Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	slog.Info("metrics server listening", "addr", addr)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("metrics server error", "err", err)
		}
	}()

	return srv
}

// Shutdown gracefully stops the metrics server (nil-safe).
func Shutdown(srv *http.Server) {
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
