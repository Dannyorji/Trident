package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// Events for a subscription must not be dropped while a previous delivery
// still holds its in-flight slot (issue #746): they wait for the slot, then are
// delivered in order even when the endpoint fails the first attempts.
func TestEventsWaitForBusySubscriptionSlotAndArriveInOrder(t *testing.T) {
	previous := allowInsecureWebhookTargets
	allowInsecureWebhookTargets = true
	t.Cleanup(func() { allowInsecureWebhookTargets = previous })

	var (
		mu        sync.Mutex
		delivered []string
		attempts  int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p webhookPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		delivered = append(delivered, p.Event.ID)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()

	sub := webhookSubscription{ID: "sub-1", Secret: "s", TargetURL: srv.URL}
	// Another replica is mid-delivery for this subscription.
	if !tryAcquireSubscriptionSlot(context.Background(), rdb, sub.ID) {
		t.Fatal("test setup: could not take the slot")
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		releaseSubscriptionSlot(context.Background(), rdb, sub.ID)
	}()

	want := []string{"e1", "e2", "e3", "e4", "e5"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, id := range want {
		deliverToSubscriptions(ctx, nil, rdb, []webhookSubscription{sub}, webhookEvent{ID: id})
	}

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != len(want) {
		t.Fatalf("delivered %v, want all of %v", delivered, want)
	}
	for i := range want {
		if delivered[i] != want[i] {
			t.Fatalf("delivery order = %v, want %v", delivered, want)
		}
	}
}
