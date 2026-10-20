package archive

import (
	"container/list"
	"sync"
)

const directoryCacheCapacity = 64

type directoryKey struct{ path, tree string }
type cachedDirectory struct {
	key      directoryKey
	messages []Message
}

type directoryCache struct {
	mu       sync.Mutex
	capacity int
	entries  map[directoryKey]*list.Element
	recency  list.List
}

func newDirectoryCache(capacity int) *directoryCache {
	return &directoryCache{capacity: capacity, entries: make(map[directoryKey]*list.Element)}
}

func (cache *directoryCache) get(key directoryKey) ([]Message, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	element, ok := cache.entries[key]
	if !ok {
		return nil, false
	}
	cache.recency.MoveToFront(element)
	return element.Value.(cachedDirectory).messages, true
}

func (cache *directoryCache) put(key directoryKey, messages []Message) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if element, ok := cache.entries[key]; ok {
		element.Value = cachedDirectory{key, messages}
		cache.recency.MoveToFront(element)
		return
	}
	element := cache.recency.PushFront(cachedDirectory{key, messages})
	cache.entries[key] = element
	if cache.recency.Len() > cache.capacity {
		oldest := cache.recency.Back()
		delete(cache.entries, oldest.Value.(cachedDirectory).key)
		cache.recency.Remove(oldest)
	}
}
