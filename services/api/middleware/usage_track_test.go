package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

// TestTrackUsage_NilChannelIsNoop guards the "tracking is optional" contract
// DBAuthConfig.UsageTrack documents: nil must never panic or block.
func TestTrackUsage_NilChannelIsNoop(t *testing.T) {
	trackUsage(nil, "some-key-id")
}

// TestTrackUsage_SendsOnChannel proves a non-nil channel actually receives
// the key id (issue #615: the tracker existed, the channel was constructed,
// but nothing ever sent on it, so request_count/last_used_at stayed 0/NULL
// forever).
func TestTrackUsage_SendsOnChannel(t *testing.T) {
	ch := make(chan string, 1)
	trackUsage(ch, "11111111-1111-1111-1111-111111111111")

	select {
	case got := <-ch:
		if got != "11111111-1111-1111-1111-111111111111" {
			t.Errorf("got %q, want the key id", got)
		}
	default:
		t.Fatal("expected trackUsage to send on the channel")
	}
}

// TestTrackUsage_FullChannelDoesNotBlock proves a saturated channel is
// dropped rather than blocking the request, matching
// NewAPIKeyUsageTracker's own "usage tracking is non-critical" contract.
func TestTrackUsage_FullChannelDoesNotBlock(t *testing.T) {
	ch := make(chan string, 1)
	ch <- "already-queued"

	done := make(chan struct{})
	go func() {
		trackUsage(ch, "would-block-without-the-default-case")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("trackUsage blocked on a full channel instead of dropping the send")
	}
}

// fakeUsageAuthDB is a minimal handlers.DB-shaped stand-in for the api_keys
// lookup NewDBAuth's DB path runs.
type fakeUsageAuthDB struct {
	id      string
	network string
}

func (d *fakeUsageAuthDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return &fakeUsageAuthRow{id: d.id, network: d.network}
}

type fakeUsageAuthRow struct {
	id      string
	network string
}

func (r *fakeUsageAuthRow) Scan(dest ...any) error {
	*dest[0].(*string) = r.id
	*dest[1].(*string) = r.network
	return nil
}

// TestNewDBAuth_TracksUsageOnDBLookupHit guards against issue #615: the
// usage tracker's channel was constructed in main.go but nothing was ever
// wired to send on it, so request_count/last_used_at stayed 0/NULL forever
// regardless of real traffic. Proves the DB-lookup success path now feeds
// DBAuthConfig.UsageTrack with the authenticated key's id.
func TestNewDBAuth_TracksUsageOnDBLookupHit(t *testing.T) {
	track := make(chan string, 1)
	db := &fakeUsageAuthDB{id: "22222222-2222-2222-2222-222222222222", network: "testnet"}

	handler := NewDBAuth(DBAuthConfig{DB: db, UsageTrack: track})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("X-API-Key", "some-db-issued-key")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	select {
	case got := <-track:
		if got != "22222222-2222-2222-2222-222222222222" {
			t.Errorf("got %q, want the authenticated key id", got)
		}
	case <-time.After(time.Second):
		t.Fatal("expected NewDBAuth to send the authenticated key id on UsageTrack")
	}
}

// TestNewDBAuth_TracksUsageOnRedisCacheHit covers the other success branch:
// a request served from the Redis cache (no DB round trip) must still feed
// the tracker, since most authenticated requests in steady state hit cache,
// not the database.
func TestNewDBAuth_TracksUsageOnRedisCacheHit(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	const key = "cached-key"
	rdb.Set(context.Background(), authRedisCacheKey(sha256KeyHash(key)), "33333333-3333-3333-3333-333333333333:testnet", authCacheTTL)

	track := make(chan string, 1)
	handler := NewDBAuth(DBAuthConfig{Redis: rdb, UsageTrack: track})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("X-API-Key", key)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	select {
	case got := <-track:
		if got != "33333333-3333-3333-3333-333333333333" {
			t.Errorf("got %q, want the cached key id", got)
		}
	case <-time.After(time.Second):
		t.Fatal("expected NewDBAuth to send the authenticated key id on UsageTrack for a cache hit")
	}
}
