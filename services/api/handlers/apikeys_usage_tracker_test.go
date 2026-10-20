package handlers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Depo-dev/trident/services/api/internal/metrics"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// flakyExecer fails the first failFor Exec calls, then records every
// successfully applied (id -> count) batch.
type flakyExecer struct {
	mu      sync.Mutex
	failFor int
	calls   int
	applied map[string]int64
}

func (f *flakyExecer) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failFor {
		return pgconn.CommandTag{}, errors.New("db down")
	}
	ids := args[0].([]string)
	counts := args[1].([]int64)
	if f.applied == nil {
		f.applied = map[string]int64{}
	}
	for i, id := range ids {
		f.applied[id] += counts[i]
	}
	return pgconn.CommandTag{}, nil
}

func (f *flakyExecer) snapshot() map[string]int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int64, len(f.applied))
	for k, v := range f.applied {
		out[k] = v
	}
	return out
}

// TestUsageTracker_FailedFlushIsRetriedAndCountsApplied is issue #753's
// "done when": a failing DB flush must not lose counts — they are applied
// once the DB recovers, and the failure is counted.
func TestUsageTracker_FailedFlushIsRetriedAndCountsApplied(t *testing.T) {
	db := &flakyExecer{failFor: 2}
	before := testutil.ToFloat64(metrics.APIKeyUsageFlushFailuresTotal)

	track, stop := newUsageTracker(db, 10*time.Millisecond)
	for _, id := range []string{"a", "a", "a", "b"} {
		track <- id
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		got := db.snapshot()
		if got["a"] == 3 && got["b"] == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("counts never applied after DB recovered: %v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()

	if got := testutil.ToFloat64(metrics.APIKeyUsageFlushFailuresTotal) - before; got < 2 {
		t.Errorf("flush failure counter delta = %v, want >= 2", got)
	}
	if got := db.snapshot(); got["a"] != 3 || got["b"] != 1 {
		t.Errorf("counts applied more than once or lost: %v", got)
	}
}
