package middleware_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Depo-dev/trident/services/api/middleware"
)

// Regression tests for the four idempotency defects found reviewing #225.
//
// The headline one is cross-tenant replay: the Redis key was built from the
// client-supplied Idempotency-Key alone, so the key namespace was shared
// across every caller. Two tenants choosing the same key with the same body
// meant the second received the first's response verbatim — and the responses
// this middleware wraps carry secrets (POST /v1/webhooks returns a plaintext
// signing secret, POST /v1/api-keys the plaintext key).

// tenantRequest issues a POST as a specific authenticated API key id.
func tenantRequest(t *testing.T, h http.Handler, tenantID, idemKey string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks", bytes.NewReader(body))
	req.Header.Set("Idempotency-Key", idemKey)
	if tenantID != "" {
		req = req.WithContext(middleware.WithAPIKeyID(req.Context(), tenantID))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestIdempotency_KeyIsScopedPerTenant is the security regression test: one
// tenant's stored response must never be replayed to another, even when both
// use an identical Idempotency-Key and an identical body.
func TestIdempotency_KeyIsScopedPerTenant(t *testing.T) {
	rdb := newIdempotencyTestRedis(t)

	// Each response embeds the caller's tenant id, standing in for the
	// per-tenant secret the real create endpoints return.
	var calls int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"secret":"whsec_for_` + middleware.APIKeyIDFromContext(r.Context()) + `"}`))
	})
	h := middleware.Idempotency(rdb, time.Minute)(handler)

	body := []byte(`{"contractId":"C1","targetUrl":"https://example.test/hook"}`)
	const sharedKey = "order-1"

	first := tenantRequest(t, h, "tenant-a", sharedKey, body)
	if first.Code != http.StatusCreated {
		t.Fatalf("tenant A: got %d, want 201", first.Code)
	}
	if !strings.Contains(first.Body.String(), "whsec_for_tenant-a") {
		t.Fatalf("tenant A body: %q", first.Body.String())
	}

	second := tenantRequest(t, h, "tenant-b", sharedKey, body)

	if strings.Contains(second.Body.String(), "whsec_for_tenant-a") {
		t.Fatalf("tenant B received tenant A's secret: %q", second.Body.String())
	}
	if second.Header().Get("Idempotent-Replayed") == "true" {
		t.Fatal("tenant B got a replay of tenant A's response")
	}
	if second.Code != http.StatusCreated {
		t.Fatalf("tenant B: got %d, want 201 (its own execution)", second.Code)
	}
	if !strings.Contains(second.Body.String(), "whsec_for_tenant-b") {
		t.Fatalf("tenant B body: %q", second.Body.String())
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("handler ran %d times, want 2 (once per tenant)", got)
	}
}

// The same tenant retrying its own key must still replay — scoping must not
// break the feature it protects.
func TestIdempotency_SameTenantStillReplays(t *testing.T) {
	rdb := newIdempotencyTestRedis(t)
	handler, calls := countingCreateHandler(t)
	h := middleware.Idempotency(rdb, time.Minute)(handler)

	body := []byte(`{"label":"x"}`)
	first := tenantRequest(t, h, "tenant-a", "k1", body)
	second := tenantRequest(t, h, "tenant-a", "k1", body)

	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("same tenant + same key must replay")
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("replay differs:\nfirst:  %q\nsecond: %q", first.Body.String(), second.Body.String())
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("handler ran %d times, want 1", got)
	}
}

// A 5xx must not be cached: the client's retry is precisely the case this
// feature exists to serve, and replaying the 500 would make the failure
// permanent for the whole TTL.
func TestIdempotency_ServerErrorIsNotCached(t *testing.T) {
	rdb := newIdempotencyTestRedis(t)

	var calls int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			http.Error(w, "transient", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h := middleware.Idempotency(rdb, time.Minute)(handler)

	body := []byte(`{"label":"y"}`)
	if got := tenantRequest(t, h, "tenant-a", "k-500", body).Code; got != http.StatusInternalServerError {
		t.Fatalf("first attempt: got %d, want 500", got)
	}

	second := tenantRequest(t, h, "tenant-a", "k-500", body)
	if second.Code != http.StatusCreated {
		t.Fatalf("retry after 5xx: got %d, want 201 — the 500 must not have been cached", second.Code)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("handler ran %d times, want 2", got)
	}
}

// A 4xx is a deterministic answer to this exact body, so it is cached and
// replayed like any other non-5xx response.
func TestIdempotency_ClientErrorIsCached(t *testing.T) {
	rdb := newIdempotencyTestRedis(t)

	var calls int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "bad input", http.StatusBadRequest)
	})
	h := middleware.Idempotency(rdb, time.Minute)(handler)

	body := []byte(`{"bad":true}`)
	tenantRequest(t, h, "tenant-a", "k-400", body)
	second := tenantRequest(t, h, "tenant-a", "k-400", body)

	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("a 4xx should replay rather than re-execute")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("handler ran %d times, want 1", got)
	}
}

// Concurrent requests sharing a key must not all execute — that is the
// double-create the feature exists to prevent. Exactly one runs the handler;
// the rest get 409 telling them to retry.
func TestIdempotency_ConcurrentSameKeyExecutesOnce(t *testing.T) {
	rdb := newIdempotencyTestRedis(t)

	var calls int32
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		<-release // hold the winner open so the others race the reservation
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h := middleware.Idempotency(rdb, time.Minute)(handler)

	const n = 5
	body := []byte(`{"label":"concurrent"}`)
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = tenantRequest(t, h, "tenant-a", "k-concurrent", body).Code
		}(i)
	}

	// Give the losers time to reach the reservation and bail before the
	// winner finishes, then let it complete.
	time.Sleep(150 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("handler ran %d times under %d concurrent identical requests, want 1", got, n)
	}

	created, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if created != 1 {
		t.Fatalf("got %d 201s, want exactly 1", created)
	}
	if conflicts != n-1 {
		t.Fatalf("got %d 409s, want %d", conflicts, n-1)
	}
}

// An in-flight reservation must still reject a *different* body as a
// conflict rather than telling the caller to retry a request that will never
// succeed under that key.
func TestIdempotency_InFlightDifferentBodyIsConflict(t *testing.T) {
	rdb := newIdempotencyTestRedis(t)

	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		w.WriteHeader(http.StatusCreated)
	})
	h := middleware.Idempotency(rdb, time.Minute)(handler)

	go func() {
		tenantRequest(t, h, "tenant-a", "k-inflight", []byte(`{"a":1}`))
	}()
	<-started

	got := tenantRequest(t, h, "tenant-a", "k-inflight", []byte(`{"a":2}`))
	close(release)

	if got.Code != http.StatusConflict {
		t.Fatalf("different body during in-flight: got %d, want 409", got.Code)
	}
	if got.Header().Get("Retry-After") != "" {
		t.Fatal("a conflicting body must not advertise Retry-After — retrying will not help")
	}
}
