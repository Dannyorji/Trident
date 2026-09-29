// Package logsampling provides fixed-rate sampling for high-volume
// debug-level log call sites (issue #239).
//
// Some debug logs fire once per event on a hot path — a per-message
// WebSocket log, a per-request RPC probe — where logging every occurrence in
// production is either prohibitively expensive to store or drowns out
// everything else at debug level. A Sampler lets that call site keep firing
// on every occurrence in code while only a fixed fraction actually reaches
// the log sink, without losing the signal entirely.
package logsampling

import "sync/atomic"

// Sampler allows through a fixed fraction (1 in N) of Allow calls.
type Sampler struct {
	n       uint64
	counter atomic.Uint64
}

// New returns a Sampler that allows through 1 in every n calls to Allow.
// n <= 1 allows every call through (no sampling).
func New(n uint64) *Sampler {
	if n == 0 {
		n = 1
	}
	return &Sampler{n: n}
}

// Allow reports whether the current call should be logged. Deterministic
// and safe for concurrent use: the 1st, (n+1)th, (2n+1)th, ... calls are
// allowed, so callers get an evenly-spaced sample rather than a random one
// that could, by chance, allow nothing through for a long stretch.
func (s *Sampler) Allow() bool {
	// (call-1) % n == 0 rather than call % n == 1: the latter is never true
	// for n == 1 (x % 1 is always 0, never 1), which would silently drop
	// every call for the no-sampling case instead of allowing all of them.
	return (s.counter.Add(1)-1)%s.n == 0
}
