package middleware

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newInvalidatorRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	srv := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return srv, rdb
}

func contractMessages(contractID string, n int) []redis.XMessage {
	msgs := make([]redis.XMessage, n)
	for i := range msgs {
		msgs[i] = redis.XMessage{
			ID:     fmt.Sprintf("%d-0", i+1),
			Values: map[string]any{"data": fmt.Sprintf(`{"contract_id":%q,"payload":{}}`, contractID)},
		}
	}
	return msgs
}

// Issue #750: a burst of events for one contract is one INCR for the batch,
// not one per event, and the version key expires.
func TestBumpCacheVersions_CoalescesPerContractPerBatch(t *testing.T) {
	srv, rdb := newInvalidatorRedis(t)

	msgs := contractMessages("CBURST", 1000)
	msgs = append(msgs, contractMessages("COTHER", 3)...)
	if err := bumpCacheVersions(context.Background(), rdb, msgs); err != nil {
		t.Fatalf("bumpCacheVersions: %v", err)
	}

	if got, _ := srv.Get(cacheVersionKey("CBURST")); got != "1" {
		t.Errorf("CBURST version = %q, want \"1\" (one INCR for the whole batch)", got)
	}
	if got, _ := srv.Get(cacheVersionKey("COTHER")); got != "1" {
		t.Errorf("COTHER version = %q, want \"1\"", got)
	}
	if ttl := srv.TTL(cacheVersionKey("CBURST")); ttl <= 0 {
		t.Errorf("version key has no TTL (%v); it would accumulate forever", ttl)
	}
}

// A version key must outlive every response-cache entry written under it, or
// a reset counter could re-issue a version number an old entry still carries.
// The longest TTL in use is 5 minutes (routes.go contractMetadataCacheTTL).
func TestCacheVersionTTL_OutlivesLongestCacheTTL(t *testing.T) {
	const longestCacheTTL = 5 * time.Minute
	if cacheVersionTTL < 12*longestCacheTTL {
		t.Fatalf("cacheVersionTTL %v must comfortably exceed the longest cache TTL %v", cacheVersionTTL, longestCacheTTL)
	}
}

// Issue #750: with several replicas running the invalidator, only the lease
// holder consumes the stream, so one event produces one INCR in total.
func TestStartCacheInvalidator_OnlyOneReplicaBumps(t *testing.T) {
	srv, rdb := newInvalidatorRedis(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const streamKey = "test:invalidator:leader"
	for i := 0; i < 3; i++ {
		go runCacheInvalidator(ctx, rdb, streamKey, 600*time.Millisecond)
	}
	waitForLeader(t, rdb)
	time.Sleep(100 * time.Millisecond) // let the leader reach its first blocking read

	if err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: streamKey,
		Values: map[string]any{"data": `{"contract_id":"CLEADER","payload":{}}`},
	}).Err(); err != nil {
		t.Fatalf("XAdd: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if v, _ := srv.Get(cacheVersionKey("CLEADER")); v != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("event never invalidated")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // a second replica would have INCRed by now
	if v, _ := srv.Get(cacheVersionKey("CLEADER")); v != "1" {
		t.Fatalf("version = %q, want \"1\": more than one replica consumed the event", v)
	}
}

// A dead leader's lease expires and another replica takes over.
func TestStartCacheInvalidator_FailsOverWhenLeaderStops(t *testing.T) {
	srv, rdb := newInvalidatorRedis(t)
	const streamKey = "test:invalidator:failover"

	leaderCtx, stopLeader := context.WithCancel(context.Background())
	followerCtx, stopFollower := context.WithCancel(context.Background())
	defer stopFollower()

	go runCacheInvalidator(leaderCtx, rdb, streamKey, 400*time.Millisecond)
	waitForLeader(t, rdb)
	go runCacheInvalidator(followerCtx, rdb, streamKey, 400*time.Millisecond)

	stopLeader()
	srv.FastForward(time.Second) // expire the dead leader's lease
	time.Sleep(500 * time.Millisecond)

	var seq atomic.Int32
	deadline := time.Now().Add(10 * time.Second)
	for {
		_ = rdb.XAdd(context.Background(), &redis.XAddArgs{
			Stream: streamKey,
			Values: map[string]any{"data": fmt.Sprintf(`{"contract_id":"CFAILOVER","payload":{"n":%d}}`, seq.Add(1))},
		}).Err()
		if v, _ := srv.Get(cacheVersionKey("CFAILOVER")); v != "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("follower never took over invalidation")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func waitForLeader(t *testing.T, rdb *redis.Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if n, _ := rdb.Exists(context.Background(), invalidatorLeaderKey).Result(); n == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no invalidator leader elected")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
