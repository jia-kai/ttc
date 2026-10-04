// Package assets caches disposable image renders separately from history snapshots.
package assets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"ttc/internal/history"
)

// Limits bound encoded source bytes and decoded pixel allocation.
const MaxBytes = 32 << 20
const MaxPixels = 16 << 20

// Decode rejects oversized or malformed images before allocating their pixels.
func Decode(data []byte) (image.Image, error) {
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("image exceeds %d bytes; resize or compress it before retrying", MaxBytes)
	}
	c, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("cannot decode image header; provide a valid PNG, JPEG or GIF: %w", err)
	}
	if c.Width <= 0 || c.Height <= 0 || c.Width > MaxPixels/c.Height {
		return nil, fmt.Errorf("image dimensions are invalid or exceed %d pixels; provide a valid image with fewer pixels", MaxPixels)
	}
	m, _, err := image.Decode(bytes.NewReader(data))
	return m, err
}

// Read snapshots a regular PNG/JPEG/GIF using bounded reads, even if it grows.
func Read(path string) ([]byte, image.Image, error) {
	// A FIFO must not block before descriptor validation. Linux ignores
	// O_NONBLOCK for regular files; validate the opened descriptor after it.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("image is not a regular file; provide a regular PNG, JPEG or GIF")
	}
	if st.Size() > MaxBytes {
		return nil, nil, fmt.Errorf("image exceeds %d bytes; resize or compress it before retrying", MaxBytes)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	m, err := Decode(b)
	return b, m, err
}

// Cache owns only derived PNG files. Original history assets are never pruned.
// Methods serialize render creation and periodically prune least-recently-used files.
type Cache struct {
	Root      string
	Limit     int64
	TTL       time.Duration
	mu        sync.Mutex
	lastPrune time.Time
	size      int64
}

// New opens a private cache with a 256 MiB limit and 30-day idle retention.
func New(root string) (*Cache, error) {
	if err := history.PrivateDir(root); err != nil {
		return nil, err
	}
	c := &Cache{Root: root, Limit: 256 << 20, TTL: 30 * 24 * time.Hour}
	return c, c.Prune()
}

// Key identifies the render inputs, including backend revision and geometry.
func Key(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}

// Get returns a cached PNG or creates it atomically. Errors are never cached.
func (c *Cache) Get(ctx context.Context, key string, create func(context.Context) (image.Image, error)) (image.Image, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(key) != 64 || strings.Trim(key, "0123456789abcdef") != "" {
		return nil, fmt.Errorf("invalid asset key")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Join(c.Root, key+".png")
	if _, m, err := Read(path); err == nil {
		now := time.Now()
		if err = os.Chtimes(path, now, now); err != nil {
			return nil, err
		}
		return m, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	m, err := create(ctx)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err = png.Encode(&b, m); err != nil {
		return nil, err
	}
	if b.Len() > MaxBytes {
		return nil, fmt.Errorf("render exceeds image byte limit")
	}
	if err = history.AtomicFile(path, b.Bytes(), 0600); err != nil {
		return nil, err
	}
	c.size += int64(b.Len())
	if c.size > c.Limit || time.Since(c.lastPrune) > time.Minute {
		if err = c.prune(); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Prune enforces the disk limit and idle TTL; safe to call between render batches.
func (c *Cache) Prune() error { c.mu.Lock(); defer c.mu.Unlock(); return c.prune() }
func (c *Cache) prune() error {
	entries, err := os.ReadDir(c.Root)
	if err != nil {
		return err
	}
	type file struct {
		path string
		size int64
		at   time.Time
	}
	var files []file
	var total int64
	now := time.Now()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".png") {
			continue
		}
		st, err := e.Info()
		if err != nil {
			return err
		}
		files = append(files, file{filepath.Join(c.Root, e.Name()), st.Size(), st.ModTime()})
		total += st.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.Before(files[j].at) })
	for _, f := range files {
		if total <= c.Limit && now.Sub(f.at) <= c.TTL {
			continue
		}
		if err := os.Remove(f.path); err != nil {
			return err
		}
		total -= f.size
	}
	c.lastPrune = now
	c.size = total
	return nil
}

// Resize fits an image inside pixel bounds, preserving aspect ratio and transparency.
// Nearest-neighbor sampling keeps the implementation and memory bounds small.
func Resize(m image.Image, maxWidth, maxHeight int) image.Image {
	b := m.Bounds()
	scale := min(float64(max(1, maxWidth))/float64(b.Dx()), float64(max(1, maxHeight))/float64(b.Dy()), 1)
	w, h := max(1, int(float64(b.Dx())*scale)), max(1, int(float64(b.Dy())*scale))
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			out.Set(x, y, m.At(b.Min.X+x*b.Dx()/w, b.Min.Y+y*b.Dy()/h))
		}
	}
	return out
}
