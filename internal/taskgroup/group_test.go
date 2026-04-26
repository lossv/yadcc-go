package taskgroup

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGroupDeduplicatesConcurrentWork(t *testing.T) {
	var group Group[string]
	var calls atomic.Int32

	inFlight := make(chan struct{}) // closed once fn begins executing
	release := make(chan struct{})  // closed to allow fn to return
	var startOnce sync.Once

	fn := func() (string, error) {
		calls.Add(1)
		startOnce.Do(func() { close(inFlight) })
		<-release
		return "value", nil
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var first, second Result[string]

	// Goroutine 1: runs fn, registers the in-flight call.
	go func() {
		defer wg.Done()
		first = group.Do("same", fn)
	}()

	// Wait until fn is executing (entry is in g.running).
	<-inFlight

	// Goroutine 2: should find the in-flight entry and wait on c.done.
	go func() {
		defer wg.Done()
		second = group.Do("same", fn)
	}()

	// Give goroutine 2 time to enter Do and block on <-c.done before we
	// release fn.  A short sleep is acceptable here; the entry is already in
	// the map so goroutine 2 only needs to acquire the mutex and reach the
	// channel receive — a few microseconds at most.
	time.Sleep(5 * time.Millisecond)
	close(release)
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
	if first.Value != "value" || second.Value != "value" {
		t.Fatalf("values = %q/%q", first.Value, second.Value)
	}
	if !second.Shared {
		t.Fatalf("second.Shared = false, want true")
	}
}

func TestGroupRunsDifferentKeysSeparately(t *testing.T) {
	var group Group[int]
	var calls atomic.Int32

	first := group.Do("a", func() (int, error) {
		return int(calls.Add(1)), nil
	})
	second := group.Do("b", func() (int, error) {
		return int(calls.Add(1)), nil
	})

	if first.Value == second.Value {
		t.Fatalf("different keys reused result: %d", first.Value)
	}
}
