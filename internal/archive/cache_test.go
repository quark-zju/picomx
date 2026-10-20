package archive

import "testing"

func TestDirectoryCacheEvictionAndVersionKeys(t *testing.T) {
	t.Parallel()
	cache := newDirectoryCache(2)
	a, b, c := directoryKey{"1", "old"}, directoryKey{"1", "new"}, directoryKey{"2/001", "old"}
	cache.put(a, []Message{{ID: 1, Size: 4}})
	cache.put(b, []Message{{ID: 2, Size: 5}})
	if got, ok := cache.get(a); !ok || got[0].ID != 1 {
		t.Fatal("old snapshot cache entry was overwritten")
	}
	cache.put(c, []Message{{ID: 1024, Size: 4}})
	if _, ok := cache.get(b); ok {
		t.Fatal("least recently used entry was not evicted")
	}
	if got, ok := cache.get(c); !ok || got[0].ID != 1024 {
		t.Fatal("tree OID key did not include directory path")
	}
	cache.put(a, []Message{{ID: 3}})
	if got, ok := cache.get(a); !ok || got[0].ID != 3 || len(cache.entries) != 2 {
		t.Fatal("cache replacement failed")
	}
}
