package cache

import "testing"

func TestBloomFilterContainsAddedKeys(t *testing.T) {
	filter := NewBloomFilterForKeys([]string{"a", "b", "c"})

	for _, key := range []string{"a", "b", "c"} {
		if !filter.MayContain(key) {
			t.Fatalf("MayContain(%q) = false, want true", key)
		}
	}
}

func TestBloomFilterUsuallyRejectsUnknownKey(t *testing.T) {
	filter := NewBloomFilter(8192, 4)
	filter.Add("known")

	if filter.MayContain("unknown") {
		t.Fatal("MayContain(unknown) = true; unexpected false positive in deterministic test")
	}
}
