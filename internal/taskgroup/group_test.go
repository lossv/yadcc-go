package taskgroup

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestGroupDeduplicatesConcurrentWork(t *testing.T) {
	var group Group[string]
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})

	fn := func() (string, error) {
		calls.Add(1)
		close(started)
		<-release
		return "value", nil
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var first Result[string]
	var second Result[string]

	go func() {
		defer wg.Done()
		first = group.Do("same", fn)
	}()
	<-started
	go func() {
		defer wg.Done()
		second = group.Do("same", fn)
	}()
	close(release)
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
	if first.Value != "value" || second.Value != "value" {
		t.Fatalf("values = %q/%q", first.Value, second.Value)
	}
	if !first.Shared || !second.Shared {
		t.Fatalf("shared = %v/%v, want both true", first.Shared, second.Shared)
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
