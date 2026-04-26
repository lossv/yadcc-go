package cache

import (
	"encoding/binary"
	"hash/fnv"
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
