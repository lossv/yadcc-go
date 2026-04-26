package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
)

// digestCache avoids re-hashing the same compiler binary on every invocation.
var digestCache struct {
	mu    sync.Mutex
	cache map[string]string // path → hex digest
}

func init() {
	digestCache.cache = make(map[string]string)
}

// Digest returns the SHA-256 hex digest of the compiler binary at path.
// Results are cached in-process (keyed by path only — suitable for
// build-session lifetime where the binary does not change mid-run).
func Digest(path string) (string, error) {
	digestCache.mu.Lock()
	if d, ok := digestCache.cache[path]; ok {
		digestCache.mu.Unlock()
		return d, nil
	}
	digestCache.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("compiler digest: open %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("compiler digest: hash %s: %w", path, err)
	}
	digest := hex.EncodeToString(h.Sum(nil))

	digestCache.mu.Lock()
	digestCache.cache[path] = digest
	digestCache.mu.Unlock()

	return digest, nil
}
