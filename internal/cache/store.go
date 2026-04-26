package cache

import (
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
}

type MemoryStore struct {
	mu    sync.RWMutex
	items map[string][]byte
	bytes int64
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{items: make(map[string][]byte)}
}

func (s *MemoryStore) Get(key string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.items[key]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), value...), nil
}

func (s *MemoryStore) Put(key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.items[key]; ok {
		s.bytes -= int64(len(old))
	}
	s.items[key] = append([]byte(nil), value...)
	s.bytes += int64(len(value))
	return nil
}

func (s *MemoryStore) Keys() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.items))
	for key := range s.items {
		keys = append(keys, key)
	}
	return keys, nil
}

func (s *MemoryStore) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Stats{Entries: len(s.items), Bytes: s.bytes}
}
