package history

import "sync"

// The cache retains immutable artifact/index metadata only, never JSON or
// rendered bodies. FIFO bounds include all chunks across all three projections.
const boundaryItemCacheEntries = 1024
const boundaryItemCacheChunks = 32768

type boundaryItemKey struct {
	lineage, hash, section, name string
	version                      int
}
type boundaryItemCache struct {
	mu      sync.Mutex
	entries map[boundaryItemKey]preparedItem
	order   []boundaryItemKey
	chunks  int
}

func itemChunks(p preparedItem) int {
	return len(p.readable.Chunks) + len(p.expanded.Chunks) + len(p.native.Chunks)
}
func (c *boundaryItemCache) get(key boundaryItemKey) (preparedItem, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.entries[key]
	return p, ok
}
func (c *boundaryItemCache) put(key boundaryItemKey, p preparedItem) {
	n := itemChunks(p)
	if n > boundaryItemCacheChunks {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[boundaryItemKey]preparedItem)
	}
	if _, ok := c.entries[key]; ok {
		return
	}
	for len(c.order) > 0 && (len(c.order) >= boundaryItemCacheEntries || c.chunks+n > boundaryItemCacheChunks) {
		old := c.order[0]
		c.order = c.order[1:]
		c.chunks -= itemChunks(c.entries[old])
		delete(c.entries, old)
	}
	c.entries[key] = p
	c.order = append(c.order, key)
	c.chunks += n
}
