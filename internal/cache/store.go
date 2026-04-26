package cache

import (
	"container/list"
	"errors"
	"sync"
)

var ErrNotFound = errors.New("cache entry not found")

type Store interface {
	Get(key string) ([]byte, error)
	Put(key string, value []byte) error
	Keys() ([]string, error)
	Stats() Stats
}

type Stats struct {
	Entries int   `json:"entries"`
	Bytes   int64 `json:"bytes"`
	Hits    int64 `json:"hits"`
	Misses  int64 `json:"misses"`
	Evicted int64 `json:"evicted"`
}

// DefaultMemoryStoreMaxBytes is the default L1 cache capacity (512 MiB).
const DefaultMemoryStoreMaxBytes = 512 << 20

// lruItem is the value stored in the linked list.
type lruItem struct {
	key   string
	value []byte
}

// MemoryStore is an LRU-evicting in-process cache.
// When the total stored bytes exceed MaxBytes, the least-recently-used entry
// is evicted.  Thread-safe.
type MemoryStore struct {
	mu       sync.Mutex
	maxBytes int64
	items    map[string]*list.Element // key → *list.Element (value = *lruItem)
	order    *list.List               // front = most recently used
	bytes    int64

	hits    int64
	misses  int64
	evicted int64
}

// NewMemoryStore returns a MemoryStore with the default capacity (512 MiB).
func NewMemoryStore() *MemoryStore {
	return NewMemoryStoreWithLimit(DefaultMemoryStoreMaxBytes)
}

// NewMemoryStoreWithLimit creates a MemoryStore with the given byte capacity.
// maxBytes <= 0 means unlimited (not recommended for production).
func NewMemoryStoreWithLimit(maxBytes int64) *MemoryStore {
	return &MemoryStore{
		maxBytes: maxBytes,
		items:    make(map[string]*list.Element),
		order:    list.New(),
	}
}

func (s *MemoryStore) Get(key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.items[key]
	if !ok {
		s.misses++
		return nil, ErrNotFound
	}
	s.order.MoveToFront(el)
	s.hits++
	item := el.Value.(*lruItem)
	return append([]byte(nil), item.value...), nil
}

func (s *MemoryStore) Put(key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if el, ok := s.items[key]; ok {
		// Update existing entry.
		old := el.Value.(*lruItem)
		s.bytes -= int64(len(old.value))
		old.value = append([]byte(nil), value...)
		s.bytes += int64(len(value))
		s.order.MoveToFront(el)
		return nil
	}

	item := &lruItem{key: key, value: append([]byte(nil), value...)}
	el := s.order.PushFront(item)
	s.items[key] = el
	s.bytes += int64(len(value))

	// Evict LRU entries until we are within budget.
	if s.maxBytes > 0 {
		for s.bytes > s.maxBytes && s.order.Len() > 1 {
			s.evictOldest()
		}
	}
	return nil
}

func (s *MemoryStore) evictOldest() {
	el := s.order.Back()
	if el == nil {
		return
	}
	item := el.Value.(*lruItem)
	s.order.Remove(el)
	delete(s.items, item.key)
	s.bytes -= int64(len(item.value))
	s.evicted++
}

func (s *MemoryStore) Keys() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.items))
	for key := range s.items {
		keys = append(keys, key)
	}
	return keys, nil
}

func (s *MemoryStore) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Entries: len(s.items),
		Bytes:   s.bytes,
		Hits:    s.hits,
		Misses:  s.misses,
		Evicted: s.evicted,
	}
}
