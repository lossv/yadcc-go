package cache

import (
	"encoding/binary"
	"hash/fnv"
	"sync"
	"time"

	"yadcc-go/internal/compress"
)

type BloomFilter struct {
	bits      []byte
	numBits   uint64
	numHashes uint32
}

func NewBloomFilter(numBits uint64, numHashes uint32) *BloomFilter {
	if numBits == 0 {
		numBits = 8
	}
	if numHashes == 0 {
		numHashes = 1
	}
	return &BloomFilter{
		bits:      make([]byte, (numBits+7)/8),
		numBits:   numBits,
		numHashes: numHashes,
	}
}

func NewBloomFilterForKeys(keys []string) *BloomFilter {
	numBits := uint64(len(keys) * 16)
	if numBits < 1024 {
		numBits = 1024
	}
	filter := NewBloomFilter(numBits, 4)
	for _, key := range keys {
		filter.Add(key)
	}
	return filter
}

// FromBytes reconstructs a BloomFilter from the bytes returned by Bytes(),
// using the given numHashes value (carried in the gRPC response).
func BloomFilterFromBytes(data []byte, numHashes uint32) *BloomFilter {
	numBits := uint64(len(data)) * 8
	f := &BloomFilter{
		bits:      append([]byte(nil), data...),
		numBits:   numBits,
		numHashes: numHashes,
	}
	return f
}

func (f *BloomFilter) Add(key string) {
	for i := uint32(0); i < f.numHashes; i++ {
		f.set(f.location(key, i))
	}
}

func (f *BloomFilter) MayContain(key string) bool {
	for i := uint32(0); i < f.numHashes; i++ {
		if !f.get(f.location(key, i)) {
			return false
		}
	}
	return true
}

func (f *BloomFilter) Bytes() []byte {
	return append([]byte(nil), f.bits...)
}

func (f *BloomFilter) NumHashes() uint32 {
	return f.numHashes
}

func (f *BloomFilter) location(key string, seed uint32) uint64 {
	h := fnv.New64a()
	var seedBytes [4]byte
	binary.LittleEndian.PutUint32(seedBytes[:], seed)
	_, _ = h.Write(seedBytes[:])
	_, _ = h.Write([]byte(key))
	return h.Sum64() % f.numBits
}

func (f *BloomFilter) set(pos uint64) {
	f.bits[pos/8] |= 1 << (pos % 8)
}

func (f *BloomFilter) get(pos uint64) bool {
	return f.bits[pos/8]&(1<<(pos%8)) != 0
}

// ---------------------------------------------------------------------------
// BloomManager: server-side incremental bloom filter tracker.
//
// It maintains:
//   - A full bloom filter rebuilt from all keys every fullRebuildInterval.
//   - A deque of recently-added keys (last recentKeyWindow) for incremental
//     responses.
//
// The gRPC FetchBloomFilter handler uses this to send incremental updates
// (newly_populated_keys) when the client last fetched recently, and a full
// filter when the client is too stale.
// ---------------------------------------------------------------------------

const (
	defaultFullRebuildInterval  = 60 * time.Second
	defaultRecentKeyWindow      = 10 * time.Minute
	defaultIncrementalThreshold = 5 * time.Minute // clients fresher than this get incremental
)

type bloomTimedKey struct {
	key string
	at  time.Time
}

type BloomManager struct {
	mu            sync.RWMutex
	store         Store
	interval      time.Duration
	recentWindow  time.Duration
	incrThreshold time.Duration

	filter        *BloomFilter
	filterBuiltAt time.Time
	recentKeys    []bloomTimedKey // append-only ring; old entries pruned on rebuild
}

func NewBloomManager(store Store) *BloomManager {
	bm := &BloomManager{
		store:         store,
		interval:      defaultFullRebuildInterval,
		recentWindow:  defaultRecentKeyWindow,
		incrThreshold: defaultIncrementalThreshold,
	}
	bm.rebuild()
	go bm.loop()
	return bm
}

func (bm *BloomManager) loop() {
	t := time.NewTicker(bm.interval)
	defer t.Stop()
	for range t.C {
		bm.rebuild()
	}
}

func (bm *BloomManager) rebuild() {
	keys, err := bm.store.Keys()
	if err != nil {
		return
	}
	filter := NewBloomFilterForKeys(keys)
	now := time.Now()

	bm.mu.Lock()
	bm.filter = filter
	bm.filterBuiltAt = now
	// Prune old recent keys.
	cutoff := now.Add(-bm.recentWindow)
	i := 0
	for i < len(bm.recentKeys) && bm.recentKeys[i].at.Before(cutoff) {
		i++
	}
	bm.recentKeys = bm.recentKeys[i:]
	bm.mu.Unlock()
}

// NotifyPut should be called whenever a new key is stored.
func (bm *BloomManager) NotifyPut(key string) {
	bm.mu.Lock()
	bm.recentKeys = append(bm.recentKeys, bloomTimedKey{key: key, at: time.Now()})
	if bm.filter != nil {
		bm.filter.Add(key)
	}
	bm.mu.Unlock()
}

// FetchResponse returns the bloom filter payload for a client.
// secondsSinceLastFetch controls whether an incremental or full response is returned.
// The bloom filter bytes are zstd-compressed.
func (bm *BloomManager) FetchResponse(secondsSinceLastFetch uint32) (incremental bool, newKeys []string, filterBytes []byte, numHashes uint32) {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	if bm.filter == nil {
		return false, nil, nil, 4
	}
	numHashes = bm.filter.NumHashes()
	sinceLastFetch := time.Duration(secondsSinceLastFetch) * time.Second

	if secondsSinceLastFetch > 0 && sinceLastFetch <= bm.incrThreshold {
		// Client is fresh enough — send only newly added keys since their last fetch.
		cutoff := time.Now().Add(-sinceLastFetch)
		for _, rk := range bm.recentKeys {
			if rk.at.After(cutoff) {
				newKeys = append(newKeys, rk.key)
			}
		}
		return true, newKeys, compress.Compress(bm.filter.Bytes()), numHashes
	}

	// Full response.
	return false, nil, compress.Compress(bm.filter.Bytes()), numHashes
}
