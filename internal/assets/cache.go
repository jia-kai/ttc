// Package assets caches disposable image renders separately from history snapshots.
package assets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"strings"
	"syscall"

	"ttc/internal/binaryinput"
	"ttc/internal/blobcache"
	"ttc/internal/imageresample"
	"ttc/internal/prompts"
)

// MaxBytes bounds encoded source bytes and rendered blobs.
const MaxBytes = blobcache.MaxBytes

// Decode rejects oversized or malformed images before allocating their pixels.
func Decode(data []byte) (image.Image, error) {
	if _, _, err := binaryinput.ImageConfig(data); err != nil {
		return nil, err
	}
	m, _, err := image.Decode(bytes.NewReader(data))
	return m, err
}

// Read snapshots a regular PNG/JPEG/GIF using bounded reads, even if it grows.
func Read(path string) ([]byte, image.Image, error) {
	b, err := ReadEncoded(path)
	if err != nil {
		return nil, nil, err
	}
	m, err := Decode(b)
	return b, m, err
}

// ReadEncoded reads a regular file without decoding its pixels. It rejects files
// larger than MaxBytes, including files that grow during the read. Call Decode
// before using uncached data as an image; this separation allows content-keyed
// thumbnail cache hits to avoid decoding the original image again.
func ReadEncoded(path string) ([]byte, error) {
	// A FIFO must not block before descriptor validation. Linux ignores
	// O_NONBLOCK for regular files; validate the opened descriptor after it.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf(prompts.ImageNotRegularFile)
	}
	if st.Size() > MaxBytes {
		return nil, fmt.Errorf(prompts.ImageTooLarge, MaxBytes)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxBytes {
		return nil, fmt.Errorf(prompts.ImageTooLarge, MaxBytes)
	}
	return b, nil
}

// Cache serializes render creation while sharing disk storage and eviction with
// all original binary files. Identical bytes share content-addressed blobs;
// render keys identify recipe references. Durable history is outside this cache.
type Cache struct {
	*blobcache.Cache
	gate chan struct{}
}

// New opens an absolute private cache root with a shared 4 GiB budget and
// 30-day idle retention. Use Default for production's shared XDG cache.
func New(root string) (*Cache, error) {
	c, err := blobcache.New(root)
	if err != nil {
		return nil, err
	}
	return &Cache{Cache: c, gate: make(chan struct{}, 1)}, nil
}

// Default opens the shared os.UserCacheDir()/ttc/assets blob cache.
func Default() (*Cache, error) {
	c, err := blobcache.Default()
	if err != nil {
		return nil, err
	}
	return &Cache{Cache: c, gate: make(chan struct{}, 1)}, nil
}

// Key identifies the render inputs, including backend revision and geometry.
func Key(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}

// Get returns a cached PNG or creates it atomically. Errors are never cached.
// Creation is serialized in this wrapper, but never holds a filesystem lock.
func (c *Cache) Get(ctx context.Context, key string, create func(context.Context) (image.Image, error)) (image.Image, error) {
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	data, err := c.Cache.Get(ctx, "render", key)
	if err == nil {
		return Decode(data)
	}
	if !errors.Is(err, blobcache.ErrMiss) {
		return nil, err
	}
	if create == nil {
		return nil, fmt.Errorf("missing render callback")
	}
	m, err := create(ctx)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if m == nil || m.Bounds().Dx() <= 0 || m.Bounds().Dy() <= 0 || m.Bounds().Dx() > binaryinput.MaxImagePixels/m.Bounds().Dy() {
		return nil, fmt.Errorf("render dimensions are invalid or exceed %d pixels", binaryinput.MaxImagePixels)
	}
	b := limitedBuffer{limit: MaxBytes}
	if err = png.Encode(contextWriter{ctx: ctx, writer: &b}, encodingImage(ctx, m)); err != nil {
		return nil, err
	}
	if err = c.Cache.Put(ctx, "render", key, b.buffer.Bytes()); err != nil {
		return nil, err
	}
	return m, nil
}

// Materialized standard images retain PNG's direct-buffer/indexed fast paths.
// Custom images may perform renderer work in At, so stop those callbacks after
// cancellation. The writer aborts output at the next write; stdlib compression
// itself is not interruptible. Wrapping standard images would box each pixel
// and prevent indexed encoding, adding substantial allocation on cache misses.
func encodingImage(ctx context.Context, m image.Image) image.Image {
	switch m.(type) {
	case *image.NRGBA, *image.NRGBA64, *image.RGBA, *image.RGBA64,
		*image.Gray, *image.Gray16, *image.Alpha, *image.Alpha16,
		*image.Paletted, *image.YCbCr, *image.NYCbCrA, *image.CMYK:
		return m
	default:
		return contextImage{Image: m, ctx: ctx}
	}
}

// Returning transparent pixels prevents further renderer calls after cancellation.
type contextImage struct {
	image.Image
	ctx context.Context
}

func (m contextImage) At(x, y int) color.Color {
	if m.ctx.Err() != nil {
		return color.NRGBA{}
	}
	return m.Image.At(x, y)
}

type contextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.writer.Write(p)
}

// Resize fits an image inside pixel bounds, preserving aspect ratio and transparency.
// Antialiased filtering preserves fine detail and observes cancellation. An image
// already inside the bounds is returned unchanged; callers must not mutate it.
func Resize(ctx context.Context, m image.Image, maxWidth, maxHeight int) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m == nil || m.Bounds().Empty() || maxWidth < 1 || maxHeight < 1 {
		return nil, fmt.Errorf("image and positive pixel bounds are required")
	}
	b := m.Bounds()
	if b.Dx() <= maxWidth && b.Dy() <= maxHeight {
		return m, nil
	}
	scale := min(float64(maxWidth)/float64(b.Dx()), float64(maxHeight)/float64(b.Dy()), 1)
	w, h := max(1, int(float64(b.Dx())*scale)), max(1, int(float64(b.Dy())*scale))
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	if err := imageresample.Draw(ctx, out, m, float64(w)/float64(b.Dx()), float64(h)/float64(b.Dy()), 0, 0); err != nil {
		return nil, err
	}
	return out, nil
}
