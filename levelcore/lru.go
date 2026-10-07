package levelcore

import "container/list"

// lru is a capacity-bounded least-recently-used cache. A zero capacity
// disables caching (every Get misses, every Put is a no-op).
type lru[K comparable, V any] struct {
	capacity int
	ll       *list.List
	items    map[K]*list.Element
}

type lruEntry[K comparable, V any] struct {
	key K
	val V
}

func newLRU[K comparable, V any](capacity int) *lru[K, V] {
	return &lru[K, V]{
		capacity: capacity,
		ll:       list.New(),
		items:    make(map[K]*list.Element),
	}
}

func (c *lru[K, V]) get(key K) (V, bool) {
	if c.capacity <= 0 {
		var zero V
		return zero, false
	}
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*lruEntry[K, V]).val, true
	}
	var zero V
	return zero, false
}

func (c *lru[K, V]) put(key K, val V) {
	if c.capacity <= 0 {
		return
	}
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*lruEntry[K, V]).val = val
		return
	}
	el := c.ll.PushFront(&lruEntry[K, V]{key, val})
	c.items[key] = el
	if c.ll.Len() > c.capacity {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.items, oldest.Value.(*lruEntry[K, V]).key)
		}
	}
}

func (c *lru[K, V]) invalidate(key K) {
	if el, ok := c.items[key]; ok {
		c.ll.Remove(el)
		delete(c.items, key)
	}
}

func (c *lru[K, V]) len() int { return c.ll.Len() }
