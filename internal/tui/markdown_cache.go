package tui

import "ttc/internal/render"

const (
	markdownCacheEntries = 64
	markdownCacheBytes   = 1 << 20 // Source bytes; AST storage is also bounded by these limits.
)

// markdownCache retains immutable transcript syntax trees independently of
// width/raster layout. FIFO eviction bounds visited chunks without retaining
// entire conversations; inspection windows own their complete document instead.
type markdownCache struct {
	documents map[string]*render.Document
	order     []string
	bytes     int
}

func (c *markdownCache) parse(source string) *render.Document {
	if document := c.documents[source]; document != nil {
		return document
	}
	document := render.ParseMarkdown(source)
	if len(source) > markdownCacheBytes {
		return document
	}
	if c.documents == nil {
		c.documents = make(map[string]*render.Document)
	}
	for len(c.order) >= markdownCacheEntries || c.bytes+len(source) > markdownCacheBytes {
		old := c.order[0]
		delete(c.documents, old)
		c.bytes -= len(old)
		c.order[0] = ""
		c.order = c.order[1:]
	}
	c.documents[source] = document
	c.order = append(c.order, source)
	c.bytes += len(source)
	return document
}
