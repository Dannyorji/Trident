package main

import (
	"context"
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

func releaseSubscriptionSlot(ctx context.Context, rdb *redis.Client, subscriptionID string) {
	rdb.Del(ctx, subscriptionLockKeyPrefix+subscriptionID)
}
