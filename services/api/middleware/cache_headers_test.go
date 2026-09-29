package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Depo-dev/trident/services/api/middleware"
)

// Regression tests for two response-cache defects found reviewing #221.

// A cache HIT must reproduce the handler's headers and status, not just its
// body. Storing the body alone made the same URL answer differently depending
// on whether the entry happened to be warm.
func TestResponseCache_HitPreservesHeadersAndStatus(t *testing.T) {
	rdb := newCacheTestRedis(t)

	var calls int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Header().Set("X-Route-Specific", "spec-v2")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h := middleware.ResponseCache(rdb, time.Minute, staticCacheKey("hdr-key", ""))(handler)

	do := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/contracts/C1/spec", nil))
		return rec
	}

	miss := do()
	if miss.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("first call X-Cache: %q, want MISS", miss.Header().Get("X-Cache"))
	}

	hit := do()
	if hit.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("second call X-Cache: %q, want HIT", hit.Header().Get("X-Cache"))
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("handler ran %d times, want 1", got)
	}

	for _, hdr := range []string{"Cache-Control", "X-Route-Specific", "Content-Type"} {
		if miss.Header().Get(hdr) != hit.Header().Get(hdr) {
			t.Errorf("%s differs between MISS and HIT: %q vs %q",
				hdr, miss.Header().Get(hdr), hit.Header().Get(hdr))
		}
	}
	if hit.Code != miss.Code {
		t.Errorf("status differs: MISS %d, HIT %d", miss.Code, hit.Code)
	}
	if hit.Body.String() != miss.Body.String() {
		t.Errorf("body differs:\nMISS %q\nHIT  %q", miss.Body.String(), hit.Body.String())
	}
}

// The singleflight winner runs on a context detached from its own request, so
// that client disconnecting cannot abort the response every waiter receives.
func TestResponseCache_WinnerCancellationDoesNotPoisonWaiters(t *testing.T) {
	rdb := newCacheTestRedis(t)

	started := make(chan struct{})
	var once sync.Once
	var sawCancel atomic.Bool

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		// Hold long enough for the winner's own request context to be
		// cancelled underneath us.
		select {
		case <-r.Context().Done():
			sawCancel.Store(true)
		case <-time.After(300 * time.Millisecond):
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h := middleware.ResponseCache(rdb, time.Minute, staticCacheKey("cancel-key", ""))(handler)

	winnerCtx, cancelWinner := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		req := httptest.NewRequest(http.MethodGet, "/v1/contracts/C1/spec", nil).WithContext(winnerCtx)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()

	<-started

	// A second, healthy client joins and waits on the same key.
	waiterRec := httptest.NewRecorder()
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.ServeHTTP(waiterRec, httptest.NewRequest(http.MethodGet, "/v1/contracts/C1/spec", nil))
	}()

	// The winner hangs up mid-flight.
	time.Sleep(50 * time.Millisecond)
	cancelWinner()
	wg.Wait()

	if sawCancel.Load() {
		t.Error("handler observed the winner's cancellation; it must run on a detached context")
	}
	if waiterRec.Code != http.StatusOK {
		t.Fatalf("waiter got %d, want 200 — an unrelated client must not fail because the winner disconnected", waiterRec.Code)
	}
	if waiterRec.Body.String() != `{"ok":true}` {
		t.Fatalf("waiter body: %q", waiterRec.Body.String())
	}
}
