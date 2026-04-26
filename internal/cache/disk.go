package cache

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type DiskStore struct {
	root string
}

func NewDiskStore(root string) (*DiskStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &DiskStore{root: root}, nil
}

func (s *DiskStore) Get(key string) ([]byte, error) {
	value, err := os.ReadFile(s.pathFor(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return value, err
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
	return os.Rename(tmpName, path)
}

func (s *DiskStore) Keys() ([]string, error) {
	var keys []string
	err := filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".entry" {
			return nil
		}
		name := strings.TrimSuffix(filepath.Base(path), ".entry")
		decoded, err := hex.DecodeString(name)
		if err != nil {
			return nil
		}
		keys = append(keys, string(decoded))
		return nil
	})
	return keys, err
}

func (s *DiskStore) Stats() Stats {
	var stats Stats
	_ = filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".entry" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		stats.Entries++
		stats.Bytes += info.Size()
		return nil
	})
	return stats
}

func (s *DiskStore) pathFor(key string) string {
	encoded := hex.EncodeToString([]byte(key))
	prefix := "00"
	if len(encoded) >= 2 {
		prefix = encoded[:2]
	}
	return filepath.Join(s.root, prefix, encoded+".entry")
}
