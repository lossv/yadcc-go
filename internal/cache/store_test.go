package cache

import (
	"errors"
	"testing"
)

func TestMemoryStorePutGet(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Put("k", []byte("value")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	got, err := store.Get("k")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if string(got) != "value" {
		t.Fatalf("Get() = %q, want value", got)
	}

	stats := store.Stats()
	if stats.Entries != 1 || stats.Bytes != int64(len("value")) {
		t.Fatalf("Stats() = %+v", stats)
	}
}

func TestMemoryStoreMiss(t *testing.T) {
	store := NewMemoryStore()
	_, err := store.Get("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestDiskStorePutGet(t *testing.T) {
	store, err := NewDiskStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewDiskStore() error = %v", err)
	}
	if err := store.Put("k", []byte("value")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	got, err := store.Get("k")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if string(got) != "value" {
		t.Fatalf("Get() = %q, want value", got)
	}

	keys, err := store.Keys()
	if err != nil {
		t.Fatalf("Keys() error = %v", err)
	}
	if len(keys) != 1 || keys[0] != "k" {
		t.Fatalf("Keys() = %v, want [k]", keys)
	}
}
