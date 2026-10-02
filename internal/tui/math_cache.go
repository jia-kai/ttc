package tui

import (
	"container/list"
	"image"
)

const decodedImageLimit = 32 << 20
const mathRasterEntries = 128

// mathRasterCache retains successful formula rasters on the UI thread. Visible
// ready entries borrow these pixels, so a raster is counted only once.
type mathRasterCache struct {
	entries map[string]*list.Element
	recent  list.List
	bytes   int
}

type mathRaster struct {
	key    string
	pixels image.Image
	bytes  int
}

func (c *mathRasterCache) get(key string) image.Image {
	if c == nil {
		return nil
	}
	if e := c.entries[key]; e != nil {
		c.recent.MoveToFront(e)
		return e.Value.(mathRaster).pixels
	}
	return nil
}

// trim returns removed keys so callers can release borrowed visible references.
func (c *mathRasterCache) trim(budget int) []string {
	var removed []string
	for c.recent.Len() > mathRasterEntries || c.bytes > max(0, budget) {
		e := c.recent.Back()
		v := e.Value.(mathRaster)
		c.bytes -= v.bytes
		delete(c.entries, v.key)
		c.recent.Remove(e)
		removed = append(removed, v.key)
	}
	return removed
}

func (c *mathRasterCache) put(key string, pixels image.Image, budget int) (bool, []string) {
	n := imageBytes(pixels)
	if pixels == nil || n > max(0, budget) {
		return false, nil
	}
	if c.entries == nil {
		c.entries = make(map[string]*list.Element)
	}
	if e := c.entries[key]; e != nil {
		c.bytes -= e.Value.(mathRaster).bytes
		c.recent.Remove(e)
	}
	c.entries[key] = c.recent.PushFront(mathRaster{key, pixels, n})
	c.bytes += n
	return true, c.trim(budget)
}

func (r *imageRenderer) trimMath(budget int) {
	if r.mathCache != nil {
		r.evictMath(r.mathCache.trim(budget))
	}
}

// Visible formulas displaced by memory pressure keep a fallback until they
// leave the viewport. Otherwise oversized visible sets endlessly evict and
// rerender each other on every frame.
func (r *imageRenderer) evictMath(keys []string) {
	for _, key := range keys {
		if r.visible[key] {
			r.ready[key] = renderReply{key: key, math: true, err: errViewportImageMemory}
		} else {
			delete(r.ready, key)
		}
	}
}

// cachedMath runs before layout or queuing, without touching the filesystem.
func (r *imageRenderer) cachedMath(key string) renderReply {
	reply := r.ready[key]
	if reply.math && reply.pixels != nil {
		r.mathCache.get(key) // Active rasters are cache hits too.
	}
	if reply.pixels == nil && reply.err == nil {
		if pixels := r.mathCache.get(key); pixels != nil {
			reply = renderReply{key: key, pixels: pixels, math: true}
			r.ready[key] = reply
		}
	}
	return reply
}
