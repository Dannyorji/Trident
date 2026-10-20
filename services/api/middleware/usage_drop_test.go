package middleware

import (
	"testing"

	"github.com/Depo-dev/trident/services/api/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestTrackUsage_DropCounterOnlyOnOverflow is issue #753's "done when": the
// drop counter moves only when the channel actually overflowed.
func TestTrackUsage_DropCounterOnlyOnOverflow(t *testing.T) {
	ch := make(chan string, 1)
	before := testutil.ToFloat64(metrics.APIKeyUsageDroppedTotal)

	trackUsage(ch, "fits")
	if got := testutil.ToFloat64(metrics.APIKeyUsageDroppedTotal) - before; got != 0 {
		t.Fatalf("drop counter moved by %v without an overflow", got)
	}

	trackUsage(ch, "overflows")
	trackUsage(ch, "overflows-again")
	if got := testutil.ToFloat64(metrics.APIKeyUsageDroppedTotal) - before; got != 2 {
		t.Fatalf("drop counter delta = %v, want 2", got)
	}
}
