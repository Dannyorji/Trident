package middleware

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditEntry represents a single audit log entry.
type AuditEntry struct {
	APIKeyID    *uuid.UUID
	Endpoint    string
	Method      string
	IP          string
	UserAgent   string
	StatusCode  int
	DurationMs  int
	ResultCount *int
	RequestID   string
	Network     string
	Timestamp   time.Time
	// AttemptedKeyPrefix and FailureReason are set only for a failed
	// authentication attempt (status 401, issue #609). APIKeyID is nil in
	// that case since the key never resolved to a row, so these are the
	// only trace of which key was tried and why. AttemptedKeyPrefix is a
	// prefix only - the same 16-character convention handlers/apikeys.go
	// uses for key_prefix at creation time - never the full attempted key.
	AttemptedKeyPrefix string
	FailureReason      string
	// AuthSource attributes a successful, non-DB-backed authentication when
	// APIKeyID is nil, for example "legacy-env" for the API_KEY_HASHES env-var
	// path (issue #616). Empty for DB-backed keys and for unauthenticated
	// (401) requests, which already have their own attribution via
	// AttemptedKeyPrefix/FailureReason.
	AuthSource string
}

// AuditWriter asynchronously writes audit log entries to PostgreSQL.
type AuditWriter struct {
	ch      chan AuditEntry
	pool    *pgxpool.Pool
	logger  *slog.Logger
	wg      sync.WaitGroup
	closed  bool
	closeMu sync.Mutex
}

// NewAuditWriter creates a new AuditWriter with a buffered channel.
// flushInterval: how often to flush batches (default 500ms)
// batchSize: max entries per batch (default 100)
// channelCap: channel capacity (default 10000)
func NewAuditWriter(pool *pgxpool.Pool, logger *slog.Logger, flushInterval time.Duration, batchSize int, channelCap int) *AuditWriter {
	if flushInterval <= 0 {
		flushInterval = 500 * time.Millisecond
	}
	if batchSize <= 0 {
		batchSize = 100
	}
	if channelCap <= 0 {
		channelCap = 10000
	}

	aw := &AuditWriter{
		ch:     make(chan AuditEntry, channelCap),
		pool:   pool,
		logger: logger,
	}

	aw.wg.Add(1)
	go aw.run(flushInterval, batchSize)

	return aw
}

// Write enqueues an audit entry. Non-blocking: drops entry with warning if channel full.
func (aw *AuditWriter) Write(entry AuditEntry) {
	aw.closeMu.Lock()
	defer aw.closeMu.Unlock()

	if aw.closed {
		return
	}

	select {
	case aw.ch <- entry:
	default:
		aw.logger.Warn("audit log channel full, dropping entry", "endpoint", entry.Endpoint)
	}
}

// Close stops the background writer and flushes pending entries.
func (aw *AuditWriter) Close() {
	aw.closeMu.Lock()
	if aw.closed {
		aw.closeMu.Unlock()
		return
	}
	aw.closed = true
	aw.closeMu.Unlock()

	close(aw.ch)
	aw.wg.Wait()
}

func (aw *AuditWriter) run(flushInterval time.Duration, batchSize int) {
	defer aw.wg.Done()

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	batch := make([]AuditEntry, 0, batchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := aw.insertBatch(ctx, batch); err != nil {
			aw.logger.Error("audit log batch insert failed", "err", err, "count", len(batch))
		}
		batch = batch[:0]
	}

	for {
		select {
		case entry, ok := <-aw.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, entry)
			if len(batch) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (aw *AuditWriter) insertBatch(ctx context.Context, batch []AuditEntry) error {
	if len(batch) == 0 {
		return nil
	}

	// Use COPY for efficient bulk insert
	rows := make([][]any, len(batch))
	for i, e := range batch {
		var apiKeyID any
		if e.APIKeyID != nil {
			apiKeyID = *e.APIKeyID
		}
		var attemptedKeyPrefix, failureReason, authSource any
		if e.AttemptedKeyPrefix != "" {
			attemptedKeyPrefix = e.AttemptedKeyPrefix
		}
		if e.FailureReason != "" {
			failureReason = e.FailureReason
		}
		if e.AuthSource != "" {
			authSource = e.AuthSource
		}
		rows[i] = []any{
			apiKeyID,
			e.Endpoint,
			e.Method,
			e.IP,
			e.UserAgent,
			e.StatusCode,
			e.DurationMs,
			e.ResultCount,
			e.RequestID,
			e.Network,
			attemptedKeyPrefix,
			failureReason,
			authSource,
			e.Timestamp,
		}
	}

	_, err := aw.pool.CopyFrom(
		ctx,
		pgx.Identifier{"audit_log"},
		[]string{"api_key_id", "endpoint", "method", "ip", "user_agent", "status_code", "duration_ms", "result_count", "request_id", "network", "attempted_key_prefix", "failure_reason", "auth_source", "ts"},
		pgx.CopyFromRows(rows),
	)
	return err
}

// ExtractClientIP extracts the client IP from the request.
// Uses X-Forwarded-For (leftmost non-private IP) when behind a proxy,
// falls back to RemoteAddr.
func ExtractClientIP(r *http.Request) string {
	// Check X-Forwarded-For header
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		ips := strings.Split(xff, ",")
		for _, ip := range ips {
			ip = strings.TrimSpace(ip)
			if ip != "" && !isPrivateIP(ip) {
				return ip
			}
		}
		// All were private, return the first one
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}

	// Check X-Real-IP header
	if realIP := r.Header.Get("X-Real-IP"); realIP != "" {
		return strings.TrimSpace(realIP)
	}

	// Fall back to RemoteAddr
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

func isPrivateIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	// Check for private ranges
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}

// auditLogContextKey is the context key for audit log data
type auditLogContextKey string

const auditLogAPIKeyIDKey auditLogContextKey = "audit_api_key_id"
const auditLogNetworkKey auditLogContextKey = "audit_network"
const auditLogAuthSourceKey auditLogContextKey = "audit_auth_source"

// WithAuditAPIKeyID stores the API key ID in the request context for audit logging.
func WithAuditAPIKeyID(ctx context.Context, apiKeyID *uuid.UUID) context.Context {
	return context.WithValue(ctx, auditLogAPIKeyIDKey, apiKeyID)
}

// AuditAPIKeyIDFromContext retrieves the audit API key UUID from the request context.
func AuditAPIKeyIDFromContext(ctx context.Context) *uuid.UUID {
	if v := ctx.Value(auditLogAPIKeyIDKey); v != nil {
		if id, ok := v.(*uuid.UUID); ok {
			return id
		}
	}
	return nil
}

// WithAuditNetwork stores the network in the request context for audit logging.
func WithAuditNetwork(ctx context.Context, network string) context.Context {
	return context.WithValue(ctx, auditLogNetworkKey, network)
}

// AuditNetworkFromContext retrieves the audit network from the request context.
func AuditNetworkFromContext(ctx context.Context) string {
	if v := ctx.Value(auditLogNetworkKey); v != nil {
		if n, ok := v.(string); ok {
			return n
		}
	}
	return ""
}

// WithAuditAuthSource stores a non-key-id attribution label in the request
// context for audit logging (issue #616). It exists for auth paths that
// authenticate a request but have no api_keys row to attach via
// WithAuditAPIKeyID, for example the legacy API_KEY_HASHES env-var path:
// without this, such a request's audit_log row carried no attribution at
// all beyond "some request happened". AuditAPIKeyIDFromContext and this are
// not mutually exclusive in general, but today only the DB-backed path sets
// the former and only the legacy path sets the latter.
func WithAuditAuthSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, auditLogAuthSourceKey, source)
}

// AuditAuthSourceFromContext retrieves the audit auth-source label from the
// request context.
func AuditAuthSourceFromContext(ctx context.Context) string {
	if v := ctx.Value(auditLogAuthSourceKey); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// responseWriter wraps http.ResponseWriter to capture status code and result count
type auditResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	resultCount *int
}

func (w *auditResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditResponseWriter) Write(b []byte) (int, error) {
	// Could parse JSON response to extract result_count for list endpoints
	// For now, we'll leave it as nil and handlers can set it via context if needed
	return w.ResponseWriter.Write(b)
}

// AuditMiddleware creates middleware that logs every request to the audit log.
func AuditMiddleware(writer *AuditWriter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			wrapped := &auditResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}

			next.ServeHTTP(wrapped, r)

			apiKeyID := AuditAPIKeyIDFromContext(r.Context())
			network := AuditNetworkFromContext(r.Context())
			authSource := AuditAuthSourceFromContext(r.Context())

			var attemptedKeyPrefix, failureReason string
			if wrapped.statusCode == http.StatusUnauthorized {
				attemptedKeyPrefix = keyPrefixForAudit(r.Header.Get("X-API-Key"))
				failureReason = authFailureReason(r.Header.Get("X-API-Key"))
			}

			entry := AuditEntry{
				APIKeyID:           apiKeyID,
				Endpoint:           r.URL.Path,
				Method:             r.Method,
				IP:                 ExtractClientIP(r),
				UserAgent:          r.Header.Get("User-Agent"),
				StatusCode:         wrapped.statusCode,
				DurationMs:         int(time.Since(start).Milliseconds()),
				ResultCount:        wrapped.resultCount,
				RequestID:          r.Header.Get("X-Request-ID"),
				Network:            network,
				AttemptedKeyPrefix: attemptedKeyPrefix,
				FailureReason:      failureReason,
				AuthSource:         authSource,
				Timestamp:          start,
			}

			writer.Write(entry)
		})
	}
}

// SetAuditResultCount allows handlers to set the result count for the audit log.
func SetAuditResultCount(w http.ResponseWriter, count int) {
	if wrapped, ok := w.(*auditResponseWriter); ok {
		wrapped.resultCount = &count
	}
}

// auditKeyPrefixLength matches handlers/apikeys.go's own key_prefix
// convention (the first 16 characters of a real "trident_<hex>" key), so an
// audited failed-auth attempt's prefix is directly comparable to a real
// key's stored prefix.
const auditKeyPrefixLength = 16

// keyPrefixForAudit returns a safe, bounded prefix of an attempted API key
// for audit logging - never the full key. Empty input (no X-API-Key header
// at all) yields an empty prefix rather than a placeholder, since "no key
// was sent" is itself recorded via failureReason.
func keyPrefixForAudit(attemptedKey string) string {
	if attemptedKey == "" {
		return ""
	}
	if len(attemptedKey) <= auditKeyPrefixLength {
		return attemptedKey
	}
	return attemptedKey[:auditKeyPrefixLength]
}

// authFailureReason gives a coarse, non-sensitive reason for a 401 suitable
// for the audit log - never anything derived from the key's actual value
// beyond whether one was present at all.
func authFailureReason(attemptedKey string) string {
	if attemptedKey == "" {
		return "missing X-API-Key header"
	}
	return "key not recognized"
}

func (w *auditResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
