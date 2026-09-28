package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Depo-dev/trident/services/api/cursor"
	"github.com/Depo-dev/trident/services/api/handlers"
	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/Depo-dev/trident/services/api/internal/metrics"
	"github.com/Depo-dev/trident/services/api/middleware"
	"github.com/Depo-dev/trident/services/api/validation"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
)

const maxWebhookAttempts = 5

// webhookListDefaultLimit/webhookListMaxLimit bound GET /v1/webhooks
// pagination (issue #220) — same shape as ListAPIKeys/ListContracts/
// ListEvents.
const (
	webhookListDefaultLimit = 50
	webhookListMaxLimit     = 200
)

type webhookSubscription struct {
	ID         string  `json:"id"`
	APIKeyID   string  `json:"apiKeyId,omitempty"`
	ContractID string  `json:"contractId"`
	Topic0     *string `json:"topic0,omitempty"`
	TargetURL  string  `json:"targetUrl"`
	Secret     string  `json:"secret,omitempty"`
	// SecondarySecret is the previous secret kept during a rotation overlap
	// window (issue #452). Deliveries signed with either the primary Secret or
	// this field verify successfully, allowing receivers to drain their queue
	// and swap their verification key before the old one expires.
	SecondarySecret *string    `json:"secondarySecret,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	PausedAt        *time.Time `json:"pausedAt,omitempty"`
	Network         string     `json:"network"`
}

type webhookEvent struct {
	ID              string         `json:"id"`
	ContractID      string         `json:"contractId"`
	LedgerSequence  int64          `json:"ledgerSequence"`
	Topic0          string         `json:"topic0"`
	Data            map[string]any `json:"data"`
	TransactionHash string         `json:"txHash"`
	Network         string         `json:"network"`
}

type webhookPayload struct {
	ID          string       `json:"id"`
	WebhookID   string       `json:"webhook_id"`
	Event       webhookEvent `json:"event"`
	Timestamp   int64        `json:"timestamp"` // Unix seconds; also used in signature
	DeliveredAt string       `json:"delivered_at"`
}

type webhookDelivery struct {
	ID             int64     `json:"id"`
	SubscriptionID string    `json:"subscriptionId"`
	EventID        string    `json:"eventId"`
	Attempt        int       `json:"attempt"`
	Attempts       int       `json:"attempts"`
	Status         string    `json:"status"`
	StatusCode     *int      `json:"statusCode,omitempty"`
	ResponseBody   string    `json:"responseBody,omitempty"`
	DeliveredAt    time.Time `json:"deliveredAt"`
	Success        bool      `json:"success"`
}

// resolveAPIKeyID returns the authenticated API key's UUID, which
// middleware.NewDBAuth resolved and attached to the request context.
//
// It previously interpreted the raw X-API-Key HEADER as an api_keys.id UUID
// — which no real key ever is, since keys are "trident_<hex>" strings — and
// then fell back to `INSERT INTO api_keys DEFAULT VALUES`, which violates
// the table's NOT NULL constraints. Every legitimate caller therefore got a
// 500 before reaching a subscription, making the documented list/create
// happy paths unreachable (caught while bringing these routes under the
// OpenAPI contract test, issue #513).
//
// Legacy env-hash keys have no database identity and cannot own webhook
// subscriptions; that is now an explicit auth error instead of a stray row
// insert.
func resolveAPIKeyID(ctx context.Context) (string, error) {
	if id := middleware.APIKeyIDFromContext(ctx); id != "" {
		return id, nil
	}
	return "", errAPIKeyNotResolvable
}

// errAPIKeyNotResolvable marks a request authenticated without a
// database-backed API key (legacy env-hash auth).
var errAPIKeyNotResolvable = errors.New(
	"webhook ownership requires a database-backed API key",
)

type webhookDeliveryResult struct {
	Success      bool
	StatusCode   int
	ResponseBody string
	Err          error
}

// signWebhookPayload signs "${timestamp}.${body}" with the subscription secret
// using HMAC-SHA256. The timestamp (Unix seconds) is also sent as the
// X-Trident-Timestamp header so receivers can verify replay attacks:
//
//	mac := hmac.New(sha256.New, []byte(secret))
//	mac.Write([]byte(fmt.Sprintf("%d.%s", timestamp, body)))
//	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
func signWebhookPayload(timestamp int64, body string, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d.%s", timestamp, body)
	return hex.EncodeToString(mac.Sum(nil))
}

// verifyWebhookSignature checks the HMAC-SHA256 signature over
// "${timestamp}.${body}" against the X-Trident-Signature header value.
//
// The header may contain a single signature ("sha256=<hex>") or two
// space-separated signatures during a rotation overlap window
// ("sha256=<new> sha256=<old>"). The verification passes if ANY of the
// supplied signatures matches — this lets receivers accept deliveries signed
// with either the new or the previous secret until they have rotated their
// verification key (issue #452).
func verifyWebhookSignature(timestamp int64, body string, signature string, secret string) bool {
	// At most two signatures are ever sent: the current secret and, during a
	// rotation overlap, the previous one. Bounding the token count stops a
	// caller from forcing an unbounded number of HMAC comparisons per request.
	const maxSignatureTokens = 2

	tokens := strings.Fields(signature)
	if len(tokens) == 0 || len(tokens) > maxSignatureTokens {
		return false
	}

	expected := "sha256=" + signWebhookPayload(timestamp, body, secret)
	matched := false
	for _, token := range tokens {
		// Every token must be well-formed; a junk prefix is not a signature.
		if !strings.HasPrefix(token, "sha256=") {
			return false
		}
		// No early return: comparing all tokens keeps the work independent of
		// which one matched.
		if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1 {
			matched = true
		}
	}
	return matched
}

// Default webhook pool size relative to the main pgxpool (defaultDBPoolSize
// in main.go): the webhook pool serves CRUD, delivery recording, and the
// worker rather than request-path reads, so it is sized smaller than the
// primary pool rather than left unbounded (issue #644).
const defaultWebhookDBPoolSize = 3
const defaultWebhookDBPoolMaxIdleConns = 1
const defaultWebhookDBPoolMaxConnLifetimeMS = 1_800_000 // 30 min, matches GO_API_DB_POOL_MAX_CONN_LIFETIME_MS default

func newDB() (*sql.DB, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, errors.New("DATABASE_URL is not set")
	}

	connConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}

	// Match the main pgxpool's statement_timeout/idle_in_transaction_session_timeout
	// discipline (issue #238) so a stuck webhook query can't hold a connection
	// indefinitely (issue #644). database/sql has no pool-level AfterConnect
	// hook, but stdlib.RegisterConnConfig lets a driver name carry a
	// pgx.ConnConfig with one, run once per new physical connection.
	stmtTimeoutMS := envIntBounded("DB_STATEMENT_TIMEOUT_MS", defaultStatementTimeoutMS, statementTimeoutMinMS, statementTimeoutMaxMS)
	idleTimeoutMS := envIntBounded("DB_IDLE_IN_TRANSACTION_TIMEOUT_MS", defaultIdleInTransactionTimeoutMS, statementTimeoutMinMS, statementTimeoutMaxMS)
	connConfig.AfterConnect = func(ctx context.Context, conn *pgconn.PgConn) error {
		if _, err := conn.Exec(ctx, fmt.Sprintf("SET statement_timeout = '%dms'", stmtTimeoutMS)).ReadAll(); err != nil {
			return fmt.Errorf("set statement_timeout: %w", err)
		}
		if _, err := conn.Exec(ctx, fmt.Sprintf("SET idle_in_transaction_session_timeout = '%dms'", idleTimeoutMS)).ReadAll(); err != nil {
			return fmt.Errorf("set idle_in_transaction_session_timeout: %w", err)
		}
		return nil
	}

	driverName := stdlib.RegisterConnConfig(connConfig)
	db, err := sql.Open(driverName, "")
	if err != nil {
		return nil, err
	}

	// database/sql defaults to unlimited open connections and no idle/lifetime
	// bound. Unlike main.go's pgxpool, this pool had never had explicit
	// limits, so it could grow without bound under load and starve Postgres
	// max_connections independently of the main pool's tuning (issue #644).
	// Sized smaller than the main pool by default: this pool serves CRUD,
	// delivery recording, and the worker, not request-path reads.
	maxOpen := int(envInt32("WEBHOOK_DB_POOL_MAX_OPEN_CONNS", defaultWebhookDBPoolSize))
	maxIdle := int(envInt32("WEBHOOK_DB_POOL_MAX_IDLE_CONNS", defaultWebhookDBPoolMaxIdleConns))
	maxLifetime := envDurationMS("WEBHOOK_DB_POOL_MAX_CONN_LIFETIME_MS", defaultWebhookDBPoolMaxConnLifetimeMS)
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(maxLifetime)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func startWebhookWorker(ctx context.Context, db *sql.DB, redisClient *redis.Client) {
	if db == nil || redisClient == nil {
		return
	}
	streamKey := os.Getenv("REDIS_STREAM_KEY")
	if streamKey == "" {
		streamKey = "trident:events"
	}
	groupName := os.Getenv("WEBHOOK_CONSUMER_GROUP")
	if groupName == "" {
		groupName = "trident-webhooks"
	}
	consumerName := os.Getenv("WEBHOOK_CONSUMER_NAME")
	if consumerName == "" {
		consumerName = "webhook-worker"
	}

	// A routine restart mid-backoff must not silently drop a pending retry
	// (issue #651) — resume anything left mid-backoff before picking up new
	// stream entries.
	resumePendingWebhookRetries(ctx, db)

	go func() {
		for {
			entries, err := redisClient.XReadGroup(ctx, &redis.XReadGroupArgs{
				Group:    groupName,
				Consumer: consumerName,
				Streams:  []string{streamKey, ">"},
				Count:    10,
				Block:    2 * time.Second,
				NoAck:    false,
			}).Result()
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, redis.Nil) {
					return
				}
				slog.Error("webhook worker read failed", "err", err)
				time.Sleep(time.Second)
				continue
			}
			for _, stream := range entries {
				for _, message := range stream.Messages {
					var event webhookEvent
					if raw, ok := message.Values["event"]; ok {
						if parsed, err := parseWebhookEvent(raw); err == nil {
							event = parsed
						} else {
							slog.Error("failed to parse webhook event", "err", err)
							if _, ackErr := redisClient.XAck(ctx, streamKey, groupName, message.ID).Result(); ackErr != nil {
								slog.Warn("failed to ack message", "err", ackErr)
							}
							continue
						}
					} else if raw, ok := message.Values["payload"]; ok {
						if parsed, err := parseWebhookEvent(raw); err == nil {
							event = parsed
						} else {
							slog.Error("failed to parse webhook payload", "err", err)
							if _, ackErr := redisClient.XAck(ctx, streamKey, groupName, message.ID).Result(); ackErr != nil {
								slog.Warn("failed to ack message", "err", ackErr)
							}
							continue
						}
					} else {
						slog.Warn("webhook worker received empty payload", "id", message.ID)
						if _, ackErr := redisClient.XAck(ctx, streamKey, groupName, message.ID).Result(); ackErr != nil {
							slog.Warn("failed to ack message", "err", ackErr)
						}
						continue
					}
					if err := processWebhookEvent(ctx, db, redisClient, event); err != nil {
						slog.Error("webhook delivery failed", "err", err)
					}
					if _, err := redisClient.XAck(ctx, streamKey, groupName, message.ID).Result(); err != nil {
						slog.Warn("failed to ack message", "err", err)
					}
				}
			}
		}
	}()
}

// webhookSecretOverlapHours returns how long a rotated (secondary) webhook
// secret stays valid, from WEBHOOK_SECRET_OVERLAP_HOURS. Defaults to 24 hours;
// a non-positive or unparseable value falls back to the default rather than
// disabling expiry, since never expiring is the unsafe direction.
func webhookSecretOverlapHours() int {
	const defaultOverlapHours = 24
	raw := os.Getenv("WEBHOOK_SECRET_OVERLAP_HOURS")
	if raw == "" {
		return defaultOverlapHours
	}
	hours, err := strconv.Atoi(raw)
	if err != nil || hours <= 0 {
		slog.Warn("invalid WEBHOOK_SECRET_OVERLAP_HOURS; using default",
			"value", raw, "default", defaultOverlapHours)
		return defaultOverlapHours
	}
	return hours
}

func startWebhookCleanupJob(ctx context.Context, db *sql.DB) {
	if db == nil {
		return
	}
	overlapHours := webhookSecretOverlapHours()
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := db.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE delivered_at < NOW() - INTERVAL '7 days'`); err != nil {
					slog.Warn("webhook cleanup failed", "err", err)
				}
				// Expire rotated secrets once the overlap window has passed
				// (issue #452). Without this the previous secret stays valid
				// forever, so rotating a compromised secret never actually
				// revokes it and the rotation provides no security benefit.
				if _, err := db.ExecContext(ctx, `
					UPDATE webhook_subscriptions
					SET secondary_secret = NULL
					WHERE secondary_secret IS NOT NULL
					  AND updated_at < NOW() - make_interval(hours => $1)
				`, overlapHours); err != nil {
					slog.Warn("webhook secret overlap expiry failed", "err", err)
				}
			}
		}
	}()
}

func parseWebhookEvent(raw any) (webhookEvent, error) {
	switch value := raw.(type) {
	case string:
		var event webhookEvent
		if err := json.Unmarshal([]byte(value), &event); err != nil {
			return webhookEvent{}, err
		}
		return event, nil
	case []byte:
		var event webhookEvent
		if err := json.Unmarshal(value, &event); err != nil {
			return webhookEvent{}, err
		}
		return event, nil
	case map[string]any:
		payload, err := json.Marshal(value)
		if err != nil {
			return webhookEvent{}, err
		}
		var event webhookEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			return webhookEvent{}, err
		}
		return event, nil
	default:
		return webhookEvent{}, fmt.Errorf("unsupported event payload type %T", raw)
	}
}

func processWebhookEvent(ctx context.Context, db *sql.DB, redisClient *redis.Client, event webhookEvent) error {
	if db == nil {
		return nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, api_key_id, contract_id, topic0, target_url, secret, secondary_secret, created_at, paused_at, network
		FROM webhook_subscriptions
		WHERE contract_id = $1
		  AND paused_at IS NULL
		  AND (topic0 IS NULL OR topic0 = $2)
		  AND network = $3
	`, event.ContractID, event.Topic0, event.Network)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	var subs []webhookSubscription
	for rows.Next() {
		var sub webhookSubscription
		var topic0 sql.NullString
		var pausedAt sql.NullTime
		var secondarySecret sql.NullString
		if err := rows.Scan(&sub.ID, &sub.APIKeyID, &sub.ContractID, &topic0, &sub.TargetURL, &sub.Secret, &secondarySecret, &sub.CreatedAt, &pausedAt, &sub.Network); err != nil {
			return err
		}
		if topic0.Valid {
			sub.Topic0 = &topic0.String
		}
		if pausedAt.Valid {
			sub.PausedAt = &pausedAt.Time
		}
		if secondarySecret.Valid {
			sub.SecondarySecret = &secondarySecret.String
		}
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Issue #454: fan out deliveries for this event's subscriptions
	// concurrently, bounded by globalDeliverySem, instead of one at a
	// time — a single slow/hanging endpoint no longer delays every other
	// subscriber matching the same event.
	var wg sync.WaitGroup
	for _, sub := range subs {
		if !tryAcquireSubscriptionSlot(ctx, redisClient, sub.ID) {
			slog.Warn("skipping delivery: previous delivery for this subscription still in flight", "subscription_id", sub.ID)
			metrics.WebhookDeliveriesTotal.WithLabelValues("skipped_in_flight").Inc()
			continue
		}
		wg.Add(1)
		globalDeliverySem <- struct{}{}
		go func(sub webhookSubscription) {
			defer wg.Done()
			defer func() { <-globalDeliverySem }()
			defer releaseSubscriptionSlot(context.Background(), redisClient, sub.ID)
			// Counted around the delivery itself so the gauge reflects work
			// actually in flight, not queue admission.
			metrics.WebhookDeliveriesInFlight.Inc()
			defer metrics.WebhookDeliveriesInFlight.Dec()
			if err := deliverSubscriptionWithRetry(ctx, db, sub, event); err != nil {
				slog.Warn("webhook delivery failed for subscription", "subscription_id", sub.ID, "err", err)
				metrics.WebhookDeliveriesTotal.WithLabelValues("failure").Inc()
				return
			}
			metrics.WebhookDeliveriesTotal.WithLabelValues("success").Inc()
		}(sub)
	}
	wg.Wait()
	return nil
}

func deliverSubscriptionWithRetry(ctx context.Context, db *sql.DB, sub webhookSubscription, event webhookEvent) error {
	return runDeliveryAttempts(ctx, db, sub, event, 1)
}

// resumeDeliveryRetry continues a delivery whose backoff was persisted by an
// earlier attempt, starting at the given attempt number rather than 1. Used
// by the restart-resume scan (issue #651) so a delivery that was mid-backoff
// when the process restarted picks up where it left off instead of starting
// the attempt count over.
func resumeDeliveryRetry(ctx context.Context, db *sql.DB, sub webhookSubscription, event webhookEvent, fromAttempt int) error {
	return runDeliveryAttempts(ctx, db, sub, event, fromAttempt)
}

func runDeliveryAttempts(ctx context.Context, db *sql.DB, sub webhookSubscription, event webhookEvent, fromAttempt int) error {
	for attempt := fromAttempt; attempt <= maxWebhookAttempts; attempt++ {
		// Record "attempted" before firing the HTTP call, so a crash
		// mid-flight leaves a trace instead of bookkeeping silently out of
		// sync with what actually happened at the receiver (issue #649).
		if err := recordWebhookAttemptStarted(ctx, db, sub.ID, event.ID, attempt); err != nil {
			slog.Warn("failed to record webhook delivery attempt start", "err", err)
		}

		start := time.Now()
		result := performWebhookDelivery(ctx, sub, event)
		durationMs := time.Since(start).Milliseconds()

		isLast := attempt == maxWebhookAttempts
		status := "failed"
		if result.Success {
			status = "success"
		} else if isLast {
			status = "dead_lettered"
		}

		var nextAttemptAt *time.Time
		if !result.Success && !isLast {
			at := time.Now().Add(time.Duration(1<<uint(attempt-1)) * time.Second)
			nextAttemptAt = &at
		}

		if err := recordWebhookDelivery(ctx, db, sub.ID, event.ID, attempt, status, result, nextAttemptAt); err != nil {
			slog.Warn("failed to record webhook delivery", "err", err)
		}

		handlers.RecordWebhookDelivery(result.Success, isLast && !result.Success, durationMs)
		if result.Success {
			return nil
		}
		if isLast {
			slog.Warn("webhook delivery dead-lettered after max attempts",
				"subscription_id", sub.ID,
				"event_id", event.ID,
				"attempts", maxWebhookAttempts,
			)
			return result.Err
		}

		sleepDuration := time.Duration(1<<uint(attempt-1)) * time.Second
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleepDuration):
		}
	}
	return nil
}

// resumePendingWebhookRetries finds deliveries left mid-backoff by a process
// restart (issue #651) — status 'failed', not dead-lettered, with a
// next_attempt_at that has already elapsed — and resumes each one at its
// next attempt number instead of leaving it stuck forever.
func resumePendingWebhookRetries(ctx context.Context, db *sql.DB) {
	if db == nil {
		return
	}
	rows, err := db.QueryContext(ctx, `
		SELECT wd.subscription_id, wd.event_id, wd.attempt
		FROM webhook_deliveries wd
		WHERE wd.status = 'failed'
		  AND wd.next_attempt_at IS NOT NULL
		  AND wd.next_attempt_at <= NOW()
		  AND wd.id = (
		      SELECT MAX(id) FROM webhook_deliveries
		      WHERE subscription_id = wd.subscription_id AND event_id = wd.event_id
		  )
	`)
	if err != nil {
		slog.Warn("failed to query pending webhook retries", "err", err)
		return
	}
	defer func() { _ = rows.Close() }()

	type pending struct {
		subscriptionID string
		eventID        string
		lastAttempt    int
	}
	var toResume []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.subscriptionID, &p.eventID, &p.lastAttempt); err != nil {
			slog.Warn("failed to scan pending webhook retry", "err", err)
			continue
		}
		toResume = append(toResume, p)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("failed to read pending webhook retries", "err", err)
		return
	}

	for _, p := range toResume {
		sub, event, err := loadSubscriptionAndEventForRetry(ctx, db, p.subscriptionID, p.eventID)
		if err != nil {
			slog.Warn("failed to load subscription/event for pending webhook retry",
				"subscription_id", p.subscriptionID, "event_id", p.eventID, "err", err)
			continue
		}
		go func(sub webhookSubscription, event webhookEvent, nextAttempt int) {
			if err := resumeDeliveryRetry(ctx, db, sub, event, nextAttempt); err != nil {
				slog.Warn("resumed webhook delivery failed", "subscription_id", sub.ID, "event_id", event.ID, "err", err)
			}
		}(sub, event, p.lastAttempt+1)
	}
}

func loadSubscriptionAndEventForRetry(ctx context.Context, db *sql.DB, subscriptionID, eventID string) (webhookSubscription, webhookEvent, error) {
	var sub webhookSubscription
	var topic0 sql.NullString
	var pausedAt sql.NullTime
	var secondarySecret sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT id, api_key_id, contract_id, topic0, target_url, secret, secondary_secret, created_at, paused_at, network
		FROM webhook_subscriptions WHERE id = $1
	`, subscriptionID).Scan(&sub.ID, &sub.APIKeyID, &sub.ContractID, &topic0, &sub.TargetURL, &sub.Secret, &secondarySecret, &sub.CreatedAt, &pausedAt, &sub.Network)
	if err != nil {
		return webhookSubscription{}, webhookEvent{}, err
	}
	if topic0.Valid {
		sub.Topic0 = &topic0.String
	}
	if pausedAt.Valid {
		sub.PausedAt = &pausedAt.Time
	}
	if secondarySecret.Valid {
		sub.SecondarySecret = &secondarySecret.String
	}

	event, err := loadSorobanEventByID(ctx, db, eventID)
	if err != nil {
		return webhookSubscription{}, webhookEvent{}, err
	}
	return sub, event, nil
}

// loadSorobanEventByID loads a soroban_events row for webhook replay/resume.
// soroban_events has no `topic0` column — the generated column is
// `topic_0` — and `data` is jsonb, not text; querying the wrong name/shape
// here previously errored on every call, which the replay handler's now
// removed silent fallback masked (issue #652).
func loadSorobanEventByID(ctx context.Context, db *sql.DB, eventID string) (webhookEvent, error) {
	var event webhookEvent
	var topic0 sql.NullString
	var data []byte
	err := db.QueryRowContext(ctx, `
		SELECT id, contract_id, ledger_sequence, topic_0, data, transaction_hash, network
		FROM soroban_events WHERE id = $1
	`, eventID).Scan(&event.ID, &event.ContractID, &event.LedgerSequence, &topic0, &data, &event.TransactionHash, &event.Network)
	if err != nil {
		return webhookEvent{}, err
	}
	if topic0.Valid {
		event.Topic0 = topic0.String
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &event.Data); err != nil {
			return webhookEvent{}, fmt.Errorf("unmarshal event data: %w", err)
		}
	}
	return event, nil
}

func performWebhookDelivery(ctx context.Context, sub webhookSubscription, event webhookEvent) webhookDeliveryResult {
	// Re-validate at delivery time, not just at subscription time: DNS can
	// change between the two, re-pointing an already-approved hostname at
	// an internal address (Issue #453).
	if err := validateWebhookTargetURL(sub.TargetURL); err != nil {
		// A subscription that passed validation at creation and fails it now
		// means the hostname was re-pointed at an internal address. That is a
		// security event, not routine delivery noise, so it gets its own
		// outcome label rather than being folded into "failure".
		metrics.WebhookDeliveriesTotal.WithLabelValues("blocked_url").Inc()
		slog.Warn("webhook delivery blocked: target URL failed revalidation",
			"subscription_id", sub.ID, "err", err)
		return webhookDeliveryResult{Err: err}
	}

	now := time.Now().Unix()
	payload, err := buildWebhookPayload(sub.ID, event, now)
	if err != nil {
		return webhookDeliveryResult{Err: err}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.TargetURL, bytes.NewReader(payload))
	if err != nil {
		return webhookDeliveryResult{Err: err}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Trident-Timestamp", strconv.FormatInt(now, 10))
	req.Header.Set("X-Trident-Signature", "sha256="+signWebhookPayload(now, string(payload), sub.Secret))
	// During a rotation overlap window both the new primary and the old
	// secondary signature are sent, space-separated. Receivers MUST accept
	// either one, allowing them to drain in-flight deliveries while they swap
	// their verification key to the new primary (issue #452).
	if sub.SecondarySecret != nil && *sub.SecondarySecret != "" {
		primary := "sha256=" + signWebhookPayload(now, string(payload), sub.Secret)
		secondary := "sha256=" + signWebhookPayload(now, string(payload), *sub.SecondarySecret)
		req.Header.Set("X-Trident-Signature", primary+" "+secondary)
	}

	client := newWebhookDeliveryHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return webhookDeliveryResult{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
	responseBody := strings.TrimSpace(string(bodyBytes))
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return webhookDeliveryResult{Success: true, StatusCode: resp.StatusCode, ResponseBody: responseBody}
	}
	return webhookDeliveryResult{Success: false, StatusCode: resp.StatusCode, ResponseBody: responseBody, Err: fmt.Errorf("webhook returned status %d", resp.StatusCode)}
}

func buildWebhookPayload(subscriptionID string, event webhookEvent, timestamp int64) ([]byte, error) {
	payload := webhookPayload{
		ID:          fmt.Sprintf("wh_%d", time.Now().UnixNano()),
		WebhookID:   subscriptionID,
		Event:       event,
		Timestamp:   timestamp,
		DeliveredAt: time.Now().UTC().Format(time.RFC3339),
	}
	return json.Marshal(payload)
}

// recordWebhookAttemptStarted inserts a placeholder row for this delivery
// attempt before the HTTP call fires, so a crash mid-flight is detectable
// (issue #649) — a row with no completion recorded means the attempt started
// and its outcome is unknown, rather than looking identical to an attempt
// that never happened. The unique (subscription_id, event_id, attempt)
// index makes this a no-op if the row already exists, e.g. on retry-resume
// after a restart finds a row this same process already started.
func recordWebhookAttemptStarted(ctx context.Context, db *sql.DB, subscriptionID string, eventID string, attempt int) error {
	if db == nil {
		return nil
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO webhook_deliveries (subscription_id, event_id, attempt, attempts, status, success)
		VALUES ($1, $2, $3, $3, 'pending', false)
		ON CONFLICT (subscription_id, event_id, attempt) DO NOTHING
	`, subscriptionID, eventID, attempt)
	return err
}

// recordWebhookDelivery records the outcome of a delivery attempt.
// ON CONFLICT updates the placeholder row recordWebhookAttemptStarted
// inserted for this attempt, rather than inserting a second row — the
// unique (subscription_id, event_id, attempt) index (issue #649) makes a
// duplicate delivery record for the same attempt impossible at the database
// level. next_attempt_at persists when the next retry is due so a restart
// mid-backoff can find and resume it (issue #651).
func recordWebhookDelivery(ctx context.Context, db *sql.DB, subscriptionID string, eventID string, attempt int, status string, result webhookDeliveryResult, nextAttemptAt *time.Time) error {
	if db == nil {
		return nil
	}
	var statusCode *int
	if result.StatusCode != 0 {
		statusCode = &result.StatusCode
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO webhook_deliveries (subscription_id, event_id, attempt, attempts, status, status_code, response_body, success, next_attempt_at)
		VALUES ($1, $2, $3, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (subscription_id, event_id, attempt) DO UPDATE SET
			status = EXCLUDED.status,
			status_code = EXCLUDED.status_code,
			response_body = EXCLUDED.response_body,
			success = EXCLUDED.success,
			next_attempt_at = EXCLUDED.next_attempt_at,
			delivered_at = NOW()
	`, subscriptionID, eventID, attempt, status, statusCode, truncateString(result.ResponseBody, 500), result.Success, nextAttemptAt)
	return err
}

func truncateString(input string, max int) string {
	if len(input) <= max {
		return input
	}
	return input[:max]
}

// listWebhooksResponse is the response envelope for GET /v1/webhooks (issue
// #220) — the same has_more/next_cursor shape as ListEventsResponse/
// ListAPIKeysResponse, so every keyset-paginated list endpoint in this API
// is walked the same way. Previously a bare JSON array with no pagination
// at all: a caller with enough subscriptions had no way to fetch them in
// bounded pages.
type listWebhooksResponse struct {
	Webhooks   []webhookSubscription `json:"webhooks"`
	HasMore    bool                  `json:"has_more"`
	NextCursor *string               `json:"next_cursor"`
}

func listWebhooksHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "database unavailable")
			return
		}
		apiKeyID, err := resolveAPIKeyID(r.Context())
		if errors.Is(err, errAPIKeyNotResolvable) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, err.Error())
			return
		}
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}

		if verr := validation.RejectUnknownParams(r.URL.Query(), "limit", "cursor"); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}

		limit := webhookListDefaultLimit
		if l := r.URL.Query().Get("limit"); l != "" {
			n, err := strconv.Atoi(l)
			if err != nil || n <= 0 || n > webhookListMaxLimit {
				httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, fmt.Sprintf("limit must be an integer between 1 and %d", webhookListMaxLimit))
				return
			}
			limit = n
		}

		// created_at is not unique on its own, so the keyset is
		// (created_at, id) — a total order with id as tiebreaker — not
		// created_at alone (issue #220), same reasoning as ListAPIKeys.
		var cursorCreatedAt, cursorID any
		if c := r.URL.Query().Get("cursor"); c != "" {
			t, id, err := cursor.DecodeKeyset(c)
			if err != nil {
				httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "cursor is not a valid pagination cursor")
				return
			}
			cursorCreatedAt, cursorID = t, id
		}

		// LIMIT $4 fetches one extra row past the page: its presence is how
		// has_more is known without a separate COUNT query.
		rows, err := db.QueryContext(r.Context(), `
			SELECT id, api_key_id, contract_id, topic0, target_url, secret, secondary_secret, created_at, paused_at, network
			FROM webhook_subscriptions
			WHERE api_key_id = $1
			  AND ($2::timestamptz IS NULL OR (created_at, id) < ($2::timestamptz, $3::uuid))
			ORDER BY created_at DESC, id DESC
			LIMIT $4
		`, apiKeyID, cursorCreatedAt, cursorID, limit+1)
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		defer func() { _ = rows.Close() }()

		var subscriptions []webhookSubscription
		for rows.Next() {
			var sub webhookSubscription
			var topic0 sql.NullString
			var pausedAt sql.NullTime
			var secondarySecret sql.NullString
			if err := rows.Scan(&sub.ID, &sub.APIKeyID, &sub.ContractID, &topic0, &sub.TargetURL, &sub.Secret, &secondarySecret, &sub.CreatedAt, &pausedAt, &sub.Network); err != nil {
				slog.Error("webhook handler error", "err", err)
				httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
				return
			}
			if topic0.Valid {
				sub.Topic0 = &topic0.String
			}
			if pausedAt.Valid {
				sub.PausedAt = &pausedAt.Time
			}
			// secondary_secret is deliberately not copied onto the response.
			// It is only needed by the delivery worker to sign during a
			// rotation overlap; returning it here would republish a secret the
			// caller is meant to be retiring. The rotate endpoint returns it
			// once, at rotation time.
			subscriptions = append(subscriptions, sub)
		}
		if subscriptions == nil {
			subscriptions = []webhookSubscription{}
		}

		hasMore := len(subscriptions) > limit
		if hasMore {
			subscriptions = subscriptions[:limit]
		}

		var nextCursor *string
		if hasMore {
			last := subscriptions[len(subscriptions)-1]
			c := cursor.EncodeKeyset(last.CreatedAt, last.ID)
			nextCursor = &c
		}

		writeJSON(w, http.StatusOK, listWebhooksResponse{
			Webhooks:   subscriptions,
			HasMore:    hasMore,
			NextCursor: nextCursor,
		})
	}
}

func createWebhookHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "database unavailable")
			return
		}
		var req struct {
			ContractID string  `json:"contractId"`
			Topic0     *string `json:"topic0"`
			TargetURL  string  `json:"targetUrl"`
			Network    string  `json:"network"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			if middleware.IsBodyTooLarge(err) {
				middleware.WriteBodyTooLarge(w, r)
				return
			}
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "invalid request body")
			return
		}
		if req.TargetURL == "" || req.ContractID == "" {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "contractId and targetUrl are required")
			return
		}
		if err := validateWebhookTargetURL(req.TargetURL); err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, err.Error())
			return
		}
		network, verr := validation.ValidateNetwork("network", req.Network, "testnet")
		if verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}
		req.Network = network
		secret, err := generateWebhookSecret()
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to generate webhook secret")
			return
		}
		apiKeyID, err := resolveAPIKeyID(r.Context())
		if errors.Is(err, errAPIKeyNotResolvable) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, err.Error())
			return
		}
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		var topic0 sql.NullString
		if req.Topic0 != nil {
			topic0 = sql.NullString{String: *req.Topic0, Valid: true}
		}
		var id string
		err = db.QueryRowContext(r.Context(), `
			INSERT INTO webhook_subscriptions (api_key_id, contract_id, topic0, target_url, secret, network)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id
		`, apiKeyID, req.ContractID, topic0, req.TargetURL, secret, req.Network).Scan(&id)
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "secret": secret, "targetUrl": req.TargetURL, "contractId": req.ContractID, "network": req.Network})
	}
}

func deleteWebhookHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if verr := validation.ValidateUUID("id", id); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}
		if db == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "database unavailable")
			return
		}
		// Deletion must be scoped to the caller's API key (#607): without
		// this, any authenticated tenant could delete another tenant's
		// webhook subscription by id alone.
		apiKeyID, err := resolveAPIKeyID(r.Context())
		if errors.Is(err, errAPIKeyNotResolvable) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, err.Error())
			return
		}
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		result, err := db.ExecContext(r.Context(), `DELETE FROM webhook_subscriptions WHERE id = $1 AND api_key_id = $2`, id, apiKeyID)
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		affected, _ := result.RowsAffected()
		if affected == 0 {
			// A subscription owned by another key looks identical to a
			// missing one - the id must not be enumerable via 403 vs 404.
			httputil.WriteErrorCtx(r.Context(), w, http.StatusNotFound, httputil.NOT_FOUND, "webhook not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func pauseWebhookHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if verr := validation.ValidateUUID("id", id); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}
		if db == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "database unavailable")
			return
		}
		// Pausing must be scoped to the caller's API key (#607): without
		// this, any authenticated tenant could pause another tenant's
		// webhook subscription by id alone.
		apiKeyID, err := resolveAPIKeyID(r.Context())
		if errors.Is(err, errAPIKeyNotResolvable) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, err.Error())
			return
		}
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		result, err := db.ExecContext(r.Context(), `UPDATE webhook_subscriptions SET paused_at = NOW() WHERE id = $1 AND api_key_id = $2`, id, apiKeyID)
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		affected, _ := result.RowsAffected()
		if affected == 0 {
			// A subscription owned by another key looks identical to a
			// missing one - the id must not be enumerable via 403 vs 404.
			httputil.WriteErrorCtx(r.Context(), w, http.StatusNotFound, httputil.NOT_FOUND, "webhook not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "paused"})
	}
}

func resumeWebhookHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if verr := validation.ValidateUUID("id", id); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}
		if db == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "database unavailable")
			return
		}
		// Resuming must be scoped to the caller's API key (#607): without
		// this, any authenticated tenant could resume another tenant's
		// webhook subscription by id alone.
		apiKeyID, err := resolveAPIKeyID(r.Context())
		if errors.Is(err, errAPIKeyNotResolvable) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, err.Error())
			return
		}
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		result, err := db.ExecContext(r.Context(), `UPDATE webhook_subscriptions SET paused_at = NULL WHERE id = $1 AND api_key_id = $2`, id, apiKeyID)
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		affected, _ := result.RowsAffected()
		if affected == 0 {
			// A subscription owned by another key looks identical to a
			// missing one - the id must not be enumerable via 403 vs 404.
			httputil.WriteErrorCtx(r.Context(), w, http.StatusNotFound, httputil.NOT_FOUND, "webhook not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "resumed"})
	}
}

func deliveriesWebhookHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if verr := validation.ValidateUUID("id", id); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}
		if db == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "database unavailable")
			return
		}
		// Listing deliveries must be scoped to the caller's API key (#607):
		// without this, any authenticated tenant could enumerate another
		// tenant's delivery history, including response_body, by subscription
		// id alone. webhook_deliveries carries no api_key_id of its own, so
		// the scope check joins through webhook_subscriptions.
		apiKeyID, err := resolveAPIKeyID(r.Context())
		if errors.Is(err, errAPIKeyNotResolvable) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, err.Error())
			return
		}
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		var subExists bool
		if err := db.QueryRowContext(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM webhook_subscriptions WHERE id = $1 AND api_key_id = $2)`,
			id, apiKeyID,
		).Scan(&subExists); err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		if !subExists {
			// A subscription owned by another key must look identical to a
			// missing one - the id must not be enumerable via 403 vs 404.
			httputil.WriteErrorCtx(r.Context(), w, http.StatusNotFound, httputil.NOT_FOUND, "webhook not found")
			return
		}
		rows, err := db.QueryContext(r.Context(), `
			SELECT id, subscription_id, event_id, attempt, attempts, status, status_code, response_body, delivered_at, success
			FROM webhook_deliveries
			WHERE subscription_id = $1
			ORDER BY delivered_at DESC
			LIMIT 100
		`, id)
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		defer func() { _ = rows.Close() }()

		var deliveries []webhookDelivery
		for rows.Next() {
			var delivery webhookDelivery
			var statusCode sql.NullInt64
			if err := rows.Scan(&delivery.ID, &delivery.SubscriptionID, &delivery.EventID, &delivery.Attempt, &delivery.Attempts, &delivery.Status, &statusCode, &delivery.ResponseBody, &delivery.DeliveredAt, &delivery.Success); err != nil {
				slog.Error("webhook handler error", "err", err)
				httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
				return
			}
			if statusCode.Valid {
				code := int(statusCode.Int64)
				delivery.StatusCode = &code
			}
			deliveries = append(deliveries, delivery)
		}
		writeJSON(w, http.StatusOK, deliveries)
	}
}

// deadLettersWebhookHandler handles GET /v1/webhooks/{id}/dead-letters.
// Returns all dead-lettered deliveries for operator inspection.
func deadLettersWebhookHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "missing webhook id")
			return
		}
		if db == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "database unavailable")
			return
		}
		// Listing dead-lettered deliveries must be scoped to the caller's API
		// key (#607): without this, any authenticated tenant could enumerate
		// another tenant's dead-letter queue by subscription id alone.
		apiKeyID, err := resolveAPIKeyID(r.Context())
		if errors.Is(err, errAPIKeyNotResolvable) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, err.Error())
			return
		}
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		var subExists bool
		if err := db.QueryRowContext(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM webhook_subscriptions WHERE id = $1 AND api_key_id = $2)`,
			id, apiKeyID,
		).Scan(&subExists); err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		if !subExists {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusNotFound, httputil.NOT_FOUND, "webhook not found")
			return
		}
		rows, err := db.QueryContext(r.Context(), `
			SELECT id, subscription_id, event_id, attempt, attempts, status, status_code, response_body, delivered_at, success
			FROM webhook_deliveries
			WHERE subscription_id = $1 AND status = 'dead_lettered'
			ORDER BY delivered_at DESC
			LIMIT 200
		`, id)
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		defer func() { _ = rows.Close() }()

		var deliveries []webhookDelivery
		for rows.Next() {
			var delivery webhookDelivery
			var statusCode sql.NullInt64
			if err := rows.Scan(&delivery.ID, &delivery.SubscriptionID, &delivery.EventID, &delivery.Attempt, &delivery.Attempts, &delivery.Status, &statusCode, &delivery.ResponseBody, &delivery.DeliveredAt, &delivery.Success); err != nil {
				slog.Error("webhook handler error", "err", err)
				httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
				return
			}
			if statusCode.Valid {
				code := int(statusCode.Int64)
				delivery.StatusCode = &code
			}
			deliveries = append(deliveries, delivery)
		}
		if deliveries == nil {
			deliveries = []webhookDelivery{}
		}
		writeJSON(w, http.StatusOK, deliveries)
	}
}

// replayDeadLetterHandler handles POST /v1/webhooks/{id}/dead-letters/{deliveryId}/replay.
// Re-attempts delivery of a single dead-lettered event and returns the result.
func replayDeadLetterHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		subID := r.PathValue("id")
		deliveryIDStr := r.PathValue("deliveryId")
		if subID == "" || deliveryIDStr == "" {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "missing webhook id or delivery id")
			return
		}
		if db == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "database unavailable")
			return
		}

		// Replaying must be scoped to the caller's API key (#607): without
		// this, any authenticated tenant could force a replay of another
		// tenant's dead-lettered delivery by id alone.
		apiKeyID, err := resolveAPIKeyID(r.Context())
		if errors.Is(err, errAPIKeyNotResolvable) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, err.Error())
			return
		}
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}

		// Load the dead-lettered delivery under a row lock (#648): FOR UPDATE
		// blocks a concurrent replay of the same delivery until this
		// transaction commits, so two concurrent requests can't both read the
		// same prevAttempts and both proceed to deliver + insert. Bumping
		// attempts here, inside the lock, is what makes the second waiter
		// see the updated count once it acquires the row.
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		defer func() { _ = tx.Rollback() }()

		var eventID string
		var prevAttempts int
		err = tx.QueryRowContext(r.Context(), `
			SELECT event_id, attempts FROM webhook_deliveries
			WHERE id = $1 AND subscription_id = $2 AND status = 'dead_lettered'
			FOR UPDATE
		`, deliveryIDStr, subID).Scan(&eventID, &prevAttempts)
		if errors.Is(err, sql.ErrNoRows) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusNotFound, httputil.NOT_FOUND, "dead-lettered delivery not found")
			return
		}
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		replayAttempt := prevAttempts + 1
		// Claim this attempt number and flip status away from dead_lettered
		// so a concurrent replay that was blocked on the row lock, once it
		// proceeds, sees status = 'failed' and 404s (not dead_lettered) —
		// only one caller ever gets to deliver for this dead-lettered row.
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE webhook_deliveries SET attempts = $2, status = 'failed' WHERE id = $1
		`, deliveryIDStr, replayAttempt); err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		if err := tx.Commit(); err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}

		// Load the subscription.
		var sub webhookSubscription
		var topic0 sql.NullString
		var pausedAt sql.NullTime
		var secondarySecret sql.NullString
		err = db.QueryRowContext(r.Context(), `
			SELECT id, api_key_id, contract_id, topic0, target_url, secret, secondary_secret, created_at, paused_at, network
			FROM webhook_subscriptions WHERE id = $1
		`, subID).Scan(&sub.ID, &sub.APIKeyID, &sub.ContractID, &topic0, &sub.TargetURL, &sub.Secret, &secondarySecret, &sub.CreatedAt, &pausedAt, &sub.Network)
		if errors.Is(err, sql.ErrNoRows) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusNotFound, httputil.NOT_FOUND, "webhook subscription not found")
			return
		}
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		if topic0.Valid {
			sub.Topic0 = &topic0.String
		}
		if secondarySecret.Valid {
			sub.SecondarySecret = &secondarySecret.String
		}
		if sub.APIKeyID != apiKeyID {
			// A subscription owned by another key must look identical to a
			// missing one - the id must not be enumerable via 403 vs 404.
			httputil.WriteErrorCtx(r.Context(), w, http.StatusNotFound, httputil.NOT_FOUND, "webhook subscription not found")
			return
		}

		// Load the original event. A replay must not proceed on a
		// zero-valued stand-in — that would deliver corrupted payload data
		// to the subscriber under a valid signature (issue #652).
		event, err := loadSorobanEventByID(r.Context(), db, eventID)
		if errors.Is(err, sql.ErrNoRows) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusNotFound, httputil.NOT_FOUND, "original event not found; cannot replay")
			return
		}
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to load original event for replay")
			return
		}

		start := time.Now()
		result := performWebhookDelivery(r.Context(), sub, event)
		handlers.RecordWebhookDelivery(result.Success, false, time.Since(start).Milliseconds())
		status := "failed"
		if result.Success {
			status = "success"
		}
		replayAttempt := prevAttempts + 1
		if err := recordWebhookDelivery(r.Context(), db, subID, eventID, replayAttempt, status, result, nil); err != nil {
		if err := recordWebhookDelivery(r.Context(), db, subID, eventID, replayAttempt, status, result); err != nil {
			slog.Warn("failed to record replay delivery", "err", err)
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"success":       result.Success,
			"status":        status,
			"attempt":       replayAttempt,
			"status_code":   result.StatusCode,
			"response_body": truncateString(result.ResponseBody, 500),
		})
	}
}

func generateWebhookSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generateWebhookSecret: %w", err)
	}
	return "whsec_" + hex.EncodeToString(b), nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// rotateWebhookSecretHandler handles POST /v1/webhooks/{id}/rotate-secret.
//
// Issues a new primary secret, demoting the current one to secondary_secret
// for the overlap window (issue #452). During the overlap window deliveries
// are signed with the new primary secret, but receivers may verify against
// either secret. The secondary is cleared automatically after
// WEBHOOK_SECRET_OVERLAP_HOURS (default 24) hours by the cleanup job, or on
// the next rotation.
//
// The response body includes both secrets so the caller can record the
// secondary during the transition:
//
//	{"id":"...", "secret":"whsec_<new>", "previousSecret":"whsec_<old>"}
func rotateWebhookSecretHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if verr := validation.ValidateUUID("id", id); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}
		if db == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "database unavailable")
			return
		}
		// Rotation must be scoped to the caller's API key. Without this an
		// authenticated caller could rotate any other tenant's webhook secret
		// and read both the old and new values back.
		apiKeyID, err := resolveAPIKeyID(r.Context())
		if errors.Is(err, errAPIKeyNotResolvable) {
			// Same canonical contract as webhook creation: legacy env-hash
			// auth carries no key identity to scope ownership to, and an
			// unresolvable key is the caller's auth mode, not a server
			// fault — 401, never a 500.
			httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, err.Error())
			return
		}
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		newSecret, err := generateWebhookSecret()
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to generate new secret")
			return
		}
		// Demote the current primary to secondary and promote the new secret in
		// one statement. RETURNING secondary_secret reads the post-update value,
		// which is exactly the secret that was primary before this call.
		var previousSecret string
		err = db.QueryRowContext(r.Context(), `
			UPDATE webhook_subscriptions
			SET secondary_secret = secret,
			    secret           = $3,
			    updated_at       = NOW()
			WHERE id = $1 AND api_key_id = $2
			RETURNING secondary_secret
		`, id, apiKeyID, newSecret).Scan(&previousSecret)
		if errors.Is(err, sql.ErrNoRows) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusNotFound, httputil.NOT_FOUND, "webhook not found")
			return
		}
		if err != nil {
			slog.Error("webhook handler error", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":             id,
			"secret":         newSecret,
			"previousSecret": previousSecret,
		})
	}
}

var deliverWebhook = func(ctx context.Context, sub webhookSubscription, event webhookEvent) error {
	result := performWebhookDelivery(ctx, sub, event)
	if result.Success {
		return nil
	}
	return result.Err
}
