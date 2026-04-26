package cache

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultDiskStoreMaxBytes is the default maximum size of the disk cache (10 GiB).
const DefaultDiskStoreMaxBytes = 10 << 30

type DiskStore struct {
	root     string
	maxBytes int64

	mu      sync.Mutex
	bytes   int64
	entries map[string]diskEntry // key → diskEntry
}

type diskEntry struct {
	path  string
	size  int64
	atime time.Time
}

func NewDiskStore(root string) (*DiskStore, error) {
	return NewDiskStoreWithLimit(root, DefaultDiskStoreMaxBytes)
}

// NewDiskStoreWithLimit creates a DiskStore capped at maxBytes.
// maxBytes <= 0 means unlimited.
func NewDiskStoreWithLimit(root string, maxBytes int64) (*DiskStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	s := &DiskStore{
		root:     root,
		maxBytes: maxBytes,
		entries:  make(map[string]diskEntry),
	}
	// Scan existing entries so we can enforce the size cap on restart.
	s.loadExisting()
	return s, nil
}

func (s *DiskStore) Get(key string) ([]byte, error) {
	path := s.pathFor(key)
	value, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// Update access time in metadata.
	s.mu.Lock()
	if e, ok := s.entries[key]; ok {
		e.atime = time.Now()
		s.entries[key] = e
	}
	s.mu.Unlock()
	return value, nil
}

func (s *DiskStore) Put(key string, value []byte) error {
	path := s.pathFor(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".entry-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(value); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}

	sz := int64(len(value))
	s.mu.Lock()
	if old, ok := s.entries[key]; ok {
		s.bytes -= old.size
	}
	s.entries[key] = diskEntry{path: path, size: sz, atime: time.Now()}
	s.bytes += sz
	s.mu.Unlock()

	s.evictIfNeeded()
	return nil
}

func (s *DiskStore) Keys() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.entries))
	for k := range s.entries {
		keys = append(keys, k)
	}
	return keys, nil
}

func (s *DiskStore) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Entries: len(s.entries),
		Bytes:   s.bytes,
	}
}

// evictIfNeeded removes the least-recently-used entries until we are within
// the configured size budget.
func (s *DiskStore) evictIfNeeded() {
	if s.maxBytes <= 0 {
		return
	}
	s.mu.Lock()
	if s.bytes <= s.maxBytes {
		s.mu.Unlock()
		return
	}

	// Sort entries by access time, oldest first.
	type kv struct {
		key string
		e   diskEntry
	}
	sorted := make([]kv, 0, len(s.entries))
	for k, e := range s.entries {
		sorted = append(sorted, kv{k, e})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].e.atime.Before(sorted[j].e.atime)
	})
	s.mu.Unlock()

	for _, item := range sorted {
		s.mu.Lock()
		if s.bytes <= s.maxBytes {
			s.mu.Unlock()
			break
		}
		e, ok := s.entries[item.key]
		if ok {
			_ = os.Remove(e.path)
			s.bytes -= e.size
			delete(s.entries, item.key)
		}
		s.mu.Unlock()
	}
}

// loadExisting scans the on-disk store directory to rebuild in-memory metadata.
func (s *DiskStore) loadExisting() {
	_ = filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".entry" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		name := strings.TrimSuffix(filepath.Base(path), ".entry")
		decoded, err := hex.DecodeString(name)
		if err != nil {
			return nil
		}
		key := string(decoded)
		atime := info.ModTime() // proxy access time from mtime on disk
		sz := info.Size()
		s.entries[key] = diskEntry{path: path, size: sz, atime: atime}
		s.bytes += sz
		return nil
	})
}

func (s *DiskStore) pathFor(key string) string {
	encoded := hex.EncodeToString([]byte(key))
	prefix := "00"
	if len(encoded) >= 2 {
		prefix = encoded[:2]
	}
	return filepath.Join(s.root, prefix, encoded+".entry")
}
