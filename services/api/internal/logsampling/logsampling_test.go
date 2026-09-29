package logsampling_test

import (
	"sync"
	"testing"

	"github.com/Depo-dev/trident/services/api/internal/logsampling"
)

func TestSampler_AllowsOneInN(t *testing.T) {
	s := logsampling.New(5)

	var allowed int
	for i := 0; i < 20; i++ {
		if s.Allow() {
			allowed++
		}
	}

	// Calls 1, 6, 11, 16 are allowed: 4 out of 20.
	if allowed != 4 {
		t.Fatalf("allowed = %d, want 4", allowed)
	}
}

func TestSampler_FirstCallIsAlwaysAllowed(t *testing.T) {
	s := logsampling.New(1000)
	if !s.Allow() {
		t.Fatal("the first call must always be allowed, regardless of n")
	}
}

func TestSampler_NAtMostOneAllowsEveryCall(t *testing.T) {
	for _, n := range []uint64{0, 1} {
		s := logsampling.New(n)
		for i := 0; i < 10; i++ {
			if !s.Allow() {
				t.Fatalf("n=%d: call %d was dropped, want every call allowed", n, i)
			}
		}
	}
}

func TestSampler_ConcurrentUseDoesNotRace(t *testing.T) {
	s := logsampling.New(3)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Allow()
		}()
	}
	wg.Wait()
}
