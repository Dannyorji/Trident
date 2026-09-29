package middleware

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// httpLatencyBuckets are the histogram bucket bounds (seconds) for
// trident_api_http_request_duration_seconds — the standard Prometheus
// client library default buckets, which comfortably span sub-millisecond
// DB-free responses through multi-second cold paths.
var httpLatencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// httpMetricKey identifies one HTTP request-latency series. pattern is the
// *registered route pattern* (e.g. "GET /v1/events/{id}"), not the raw
// request path, so path parameters (event IDs, etc.) never blow up
// cardinality.
type httpMetricKey struct {
	method  string
	pattern string
	status  string
}

type httpMetricSeries struct {
	bucketCounts []uint64 // cumulative: bucketCounts[i] = count of observations <= httpLatencyBuckets[i]
	sum          float64
	count        uint64
}

var (
	httpMetricsMu   sync.Mutex
	httpMetricsData = map[httpMetricKey]*httpMetricSeries{}
)

func recordHTTPRequest(method, pattern, status string, seconds float64) {
	key := httpMetricKey{method: method, pattern: pattern, status: status}

	httpMetricsMu.Lock()
	defer httpMetricsMu.Unlock()

	series, ok := httpMetricsData[key]
	if !ok {
		series = &httpMetricSeries{bucketCounts: make([]uint64, len(httpLatencyBuckets))}
		httpMetricsData[key] = series
	}
	for i, bound := range httpLatencyBuckets {
		if seconds <= bound {
			series.bucketCounts[i]++
		}
	}
	series.sum += seconds
	series.count++
}

// metricsResponseWriter captures the status code written by the handler.
type metricsResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *metricsResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *metricsResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

// PrometheusHTTP records request count and latency for every request, keyed
// by method, registered route pattern, and status code.
//
// Must wrap the ServeMux directly, with no other middleware in between: it
// reads r.Pattern (the route ServeMux matched) after calling next, and
// ServeMux only sets that field on the exact *http.Request pointer it
// received — a middleware that swaps in a new request via r.WithContext
// before mux dispatch would leave the outer copy's Pattern empty.
func PrometheusHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &metricsResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		pattern := r.Pattern
		if pattern == "" {
			pattern = r.URL.Path
		}
		recordHTTPRequest(r.Method, pattern, strconv.Itoa(wrapped.statusCode), time.Since(start).Seconds())
	})
}

// WriteHTTPMetrics renders trident_api_http_requests_total and
// trident_api_http_request_duration_seconds in Prometheus text format.
func WriteHTTPMetrics(w io.Writer) {
	_, _ = fmt.Fprint(w, "# HELP trident_api_http_requests_total Total HTTP requests received.\n")
	_, _ = fmt.Fprint(w, "# TYPE trident_api_http_requests_total counter\n")
	_, _ = fmt.Fprint(w, "# HELP trident_api_http_request_duration_seconds HTTP request latency in seconds.\n")
	_, _ = fmt.Fprint(w, "# TYPE trident_api_http_request_duration_seconds histogram\n")

	httpMetricsMu.Lock()
	defer httpMetricsMu.Unlock()

	for key, series := range httpMetricsData {
		for i, bound := range httpLatencyBuckets {
			_, _ = fmt.Fprintf(w, "trident_api_http_request_duration_seconds_bucket{method=%q,route=%q,status=%q,le=%q} %d\n",
				key.method, key.pattern, key.status, formatBucketBound(bound), series.bucketCounts[i])
		}
		_, _ = fmt.Fprintf(w, "trident_api_http_request_duration_seconds_bucket{method=%q,route=%q,status=%q,le=\"+Inf\"} %d\n",
			key.method, key.pattern, key.status, series.count)
		_, _ = fmt.Fprintf(w, "trident_api_http_request_duration_seconds_sum{method=%q,route=%q,status=%q} %g\n",
			key.method, key.pattern, key.status, series.sum)
		_, _ = fmt.Fprintf(w, "trident_api_http_request_duration_seconds_count{method=%q,route=%q,status=%q} %d\n",
			key.method, key.pattern, key.status, series.count)
		_, _ = fmt.Fprintf(w, "trident_api_http_requests_total{method=%q,route=%q,status=%q} %d\n",
			key.method, key.pattern, key.status, series.count)
	}
}

func formatBucketBound(bound float64) string {
	return strconv.FormatFloat(bound, 'g', -1, 64)
}
