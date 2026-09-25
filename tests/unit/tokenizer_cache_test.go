package unit

import (
	"container/list"
	"fmt"
	"sync"
	"testing"
)

type lruEntry struct {
	key   string
	value []int
	bytes int
}

type BoundedTokenizerCache struct {
	mu       sync.Mutex
	capacity int
	maxBytes int
	curBytes int
	items    map[string]*list.Element
	evict    *list.List
}

func NewBoundedTokenizerCache(capacity int, maxBytes int) *BoundedTokenizerCache {
	return &BoundedTokenizerCache{
		capacity: capacity,
		maxBytes: maxBytes,
		items:    make(map[string]*list.Element),
		evict:    list.New(),
	}
}

func (c *BoundedTokenizerCache) Get(key string) ([]int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.items[key]; ok {
		c.evict.MoveToFront(elem)
		return elem.Value.(*lruEntry).value, true
	}
	return nil, false
}

func (c *BoundedTokenizerCache) Put(key string, tokens []int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	itemBytes := len(key) + len(tokens)*4

	if elem, ok := c.items[key]; ok {
		c.evict.MoveToFront(elem)
		ent := elem.Value.(*lruEntry)
		c.curBytes -= ent.bytes
		ent.value = tokens
		ent.bytes = itemBytes
		c.curBytes += itemBytes
		return
	}

	for (len(c.items) >= c.capacity || (c.curBytes+itemBytes > c.maxBytes && c.curBytes > 0)) && c.evict.Len() > 0 {
		back := c.evict.Back()
		if back == nil {
			break
		}
		ent := back.Value.(*lruEntry)
		delete(c.items, ent.key)
		c.evict.Remove(back)
		c.curBytes -= ent.bytes
	}

	ent := &lruEntry{key: key, value: tokens, bytes: itemBytes}
	elem := c.evict.PushFront(ent)
	c.items[key] = elem
	c.curBytes += itemBytes
}

func (c *BoundedTokenizerCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

func TestTokenizerBoundedLRU(t *testing.T) {
	// 容量限制为 5 个，最大字节 500
	cache := NewBoundedTokenizerCache(5, 500)

	for i := 1; i <= 5; i++ {
		cache.Put(fmt.Sprintf("text-%d", i), []int{i, i * 10})
	}
	if cache.Len() != 5 {
		t.Fatalf("expected 5 items, got %d", cache.Len())
	}

	// 访问 text-1，使其成为最常使用项
	if _, ok := cache.Get("text-1"); !ok {
		t.Fatalf("expected text-1 to exist")
	}

	// 插入第 6 项，应淘汰 text-2（LRU）
	cache.Put("text-6", []int{6, 60})
	if cache.Len() != 5 {
		t.Fatalf("expected capped length 5, got %d", cache.Len())
	}

	// text-1 应仍然存在
	if _, ok := cache.Get("text-1"); !ok {
		t.Fatalf("text-1 should still exist because it was accessed recently")
	}

	// text-2 应已被淘汰
	if _, ok := cache.Get("text-2"); ok {
		t.Fatalf("text-2 should have been evicted by LRU")
	}
}
