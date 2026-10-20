package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Issue #454: deliveries for different subscriptions matching the same
// event used to run one at a time in a single loop, so a single hanging
// endpoint delayed every other subscriber behind it. globalDeliverySem
// bounds total concurrent deliveries across the process.
var globalDeliverySem = make(chan struct{}, 20)

// subscriptionLockTTL bounds how long a single-flight lock is held before
// it expires on its own, so a crashed replica mid-delivery can't wedge a
// subscription's slot forever.
const subscriptionLockTTL = 2 * time.Minute

const subscriptionLockKeyPrefix = "webhook:inflight:"

// tryAcquireSubscriptionSlot reports whether a delivery for subscriptionID
// may proceed now (no delivery already in flight for it anywhere in the
// fleet), reserving the slot if so. Backed by a Redis lock (issue #650)
// rather than in-process state, so the single-flight-per-subscription
// invariant holds across multiple API replicas, not just within one
// process. Pairs with releaseSubscriptionSlot.
func tryAcquireSubscriptionSlot(ctx context.Context, rdb *redis.Client, subscriptionID string) bool {
	ok, err := rdb.SetNX(ctx, subscriptionLockKeyPrefix+subscriptionID, "1", subscriptionLockTTL).Result()
	if err != nil {
		return false
	}
	return ok
}

// subscriptionSlotPoll is how often a delivery waiting for a busy slot retries.
var subscriptionSlotPoll = 50 * time.Millisecond

// waitForSubscriptionSlot blocks until the subscription's slot is acquired, so
// an event that arrives while a previous delivery is still retrying is queued
// behind it instead of being dropped (issue #746). The slot's TTL bounds the
// wait if a holder dies. It returns false only when ctx ends first; if Redis
// itself errors it proceeds without the slot rather than losing the event.
func waitForSubscriptionSlot(ctx context.Context, rdb *redis.Client, subscriptionID string) bool {
	ticker := time.NewTicker(subscriptionSlotPoll)
	defer ticker.Stop()
	for {
		ok, err := rdb.SetNX(ctx, subscriptionLockKeyPrefix+subscriptionID, "1", subscriptionLockTTL).Result()
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			slog.Warn("webhook in-flight slot unavailable, delivering without it", "subscription_id", subscriptionID, "err", err)
			return true
		}
		if ok {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func releaseSubscriptionSlot(ctx context.Context, rdb *redis.Client, subscriptionID string) {
	rdb.Del(ctx, subscriptionLockKeyPrefix+subscriptionID)
}
