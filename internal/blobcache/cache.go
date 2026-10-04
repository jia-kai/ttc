// Package blobcache stores disposable encoded image bytes in a shared filesystem
// cache. Originals and renders share one idle TTL and least-recently-used budget.
package blobcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"ttc/internal/filelock"
)

// MaxBytes bounds each encoded blob, including reads of externally changed files.
const MaxBytes = 32 << 20

// ErrMiss identifies an absent or expired blob, not a cache-storage failure.
var ErrMiss = errors.New("blob cache miss")

// Cache coordinates operations with short-lived cancelable filesystem locks.
// Entries are never pinned. Configure Limit and TTL before concurrent use.
type Cache struct {
	Root      string        // Absolute directory containing private blob files and .lock.
	Limit     int64         // Total blob/staging bytes; defaults to 4 GiB. Lock metadata is excluded.
	TTL       time.Duration // Idle retention; defaults to 30 days.
	lastPrune atomic.Int64
}

var defaults struct {
	sync.Mutex
	root  string
	cache *Cache
}

// Default opens os.UserCacheDir()/ttc/assets. Repeated calls for the same root
// reuse the cache without scanning its contents; operations enforce retention.
func Default() (*Cache, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	root = filepath.Join(root, "ttc", "assets")
	defaults.Lock()
	if defaults.cache != nil && defaults.root == root {
		c := defaults.cache
		defaults.Unlock()
		return c, nil
	}
	defaults.Unlock()
	// Initialization performs no flock wait, and filesystem operations never
	// hold the defaults mutex. Get/Put/Prune acquire the cancelable lock.
	c, err := New(root)
	if err != nil {
		return nil, err
	}
	defaults.Lock()
	defer defaults.Unlock()
	if defaults.cache != nil && defaults.root == root {
		return defaults.cache, nil
	}
	defaults.root, defaults.cache = root, c
	return c, nil
}

// New opens a private absolute directory, without a full scan or lifetime lock.
// The cache has a 4 GiB total hard cap and 30-day idle TTL. Initialization does
// not acquire the filesystem lock; operations acquire it with their context.
func New(root string) (*Cache, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("blob cache root must be absolute")
	}
	root = filepath.Clean(root)
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	st, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("blob cache root is not a directory")
	}
	if err := os.Chmod(root, 0700); err != nil {
		return nil, err
	}
	if st, err := os.Lstat(filepath.Join(root, ".lock")); err == nil {
		if err := regular(st); err != nil {
			return nil, fmt.Errorf("invalid cache lock: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return &Cache{Root: root, Limit: 4 << 30, TTL: 30 * 24 * time.Hour}, nil
}

func blobName(kind, key string) (string, error) {
	if kind != "original" && kind != "render" {
		return "", fmt.Errorf("invalid blob kind %q", kind)
	}
	if len(key) != 64 || strings.Trim(key, "0123456789abcdef") != "" {
		return "", fmt.Errorf("invalid blob key")
	}
	return kind + "-" + key + ".blob", nil
}

func regular(st os.FileInfo) error {
	if !st.Mode().IsRegular() {
		return fmt.Errorf("blob cache entry is not a regular file")
	}
	if stat, ok := st.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
		return fmt.Errorf("blob cache entry has multiple hard links")
	}
	return nil
}

func (c *Cache) lock(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(c.Root) || c.Limit < 0 || c.TTL < 0 {
		return nil, fmt.Errorf("invalid blob cache configuration")
	}
	st, err := os.Lstat(c.Root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("blob cache root is not a directory")
	}
	f, err := filelock.Acquire(ctx, filepath.Join(c.Root, ".lock"))
	if err != nil {
		return nil, err
	}
	fail := func(err error) (func(), error) { f.Close(); return nil, err }
	st, err = f.Stat()
	if err != nil {
		return fail(err)
	}
	if err = regular(st); err != nil {
		return fail(err)
	}
	return func() { _ = f.Close() }, nil
}

// Get returns bounded bytes and touches idle/LRU time. An absent or expired
// entry returns an error matching ErrMiss; storage/unsafe/oversized files fail.
// Reads opportunistically prune the whole cache at most once per minute.
func (c *Cache) Get(ctx context.Context, kind, key string) ([]byte, error) {
	name, err := blobName(kind, key)
	if err != nil {
		return nil, err
	}
	unlock, err := c.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	path := filepath.Join(c.Root, name)
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			if pruneErr := c.maybePrune(ctx); pruneErr != nil {
				return nil, pruneErr
			}
			return nil, fmt.Errorf("%w: %s", ErrMiss, path)
		}
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err = regular(st); err != nil {
		return nil, err
	}
	if st.Size() > MaxBytes {
		return nil, fmt.Errorf("blob exceeds %d bytes", MaxBytes)
	}
	if time.Since(st.ModTime()) > c.TTL {
		if err = os.Remove(path); err != nil {
			return nil, err
		}
		if err = c.maybePrune(ctx); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: expired %s", ErrMiss, path)
	}
	b, err := readBounded(ctx, f, st.Size())
	if err != nil {
		return nil, err
	}
	now := syscall.NsecToTimeval(time.Now().UnixNano())
	if err = syscall.Futimes(int(f.Fd()), []syscall.Timeval{now, now}); err != nil {
		return nil, err
	}
	if err = c.maybePrune(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

func readBounded(ctx context.Context, f *os.File, size int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Preallocate from the validated descriptor's size rather than repeatedly
	// copying large uploads. Small formula blobs need no 64 KiB read buffer.
	capacity := int(min(max(size, 0), MaxBytes))
	b := make([]byte, 0, capacity)
	buf := make([]byte, min(max(capacity+1, 4<<10), 64<<10))
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := f.Read(buf)
		if len(b)+n > MaxBytes {
			return nil, fmt.Errorf("blob exceeds %d bytes", MaxBytes)
		}
		b = append(b, buf[:n]...)
		if err == io.EOF {
			return b, ctx.Err()
		}
		if err != nil {
			return nil, err
		}
	}
}

// Put atomically publishes bytes after removing any old replacement and
// TTL/LRU eviction makes room for staging. Cancellation may leave a replacement
// absent, but never exposes partial bytes. A blob larger than Limit evicts
// itself, returning success without persisting it.
// The 32 MiB per-blob bound remains an error, independent of the total budget.
func (c *Cache) Put(ctx context.Context, kind, key string, data []byte) error {
	name, err := blobName(kind, key)
	if err != nil {
		return err
	}
	if len(data) > MaxBytes {
		return fmt.Errorf("blob exceeds %d bytes", MaxBytes)
	}
	unlock, err := c.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	budget := c.Limit - int64(len(data))
	if budget < 0 {
		budget = 0
	}
	if err = c.prune(ctx, budget, name); err != nil {
		return err
	}
	path := filepath.Join(c.Root, name)
	if int64(len(data)) > c.Limit {
		return ctx.Err()
	}
	f, err := os.CreateTemp(c.Root, ".blob-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	for remaining := data; len(remaining) > 0; {
		if err = ctx.Err(); err != nil {
			return err
		}
		n, writeErr := f.Write(remaining[:min(len(remaining), 64<<10)])
		if writeErr != nil {
			return writeErr
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		remaining = remaining[n:]
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Prune enforces the combined total budget and idle TTL under a cancelable lock.
func (c *Cache) Prune(ctx context.Context) error {
	unlock, err := c.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return c.prune(ctx, c.Limit, "")
}

func (c *Cache) maybePrune(ctx context.Context) error {
	if time.Since(time.Unix(0, c.lastPrune.Load())) < time.Minute {
		return ctx.Err()
	}
	return c.prune(ctx, c.Limit, "")
}

func (c *Cache) prune(ctx context.Context, budget int64, replacing string) error {
	entries, err := os.ReadDir(c.Root)
	if err != nil {
		return err
	}
	type entry struct {
		path string
		size int64
		at   time.Time
	}
	var files []entry
	var total int64
	now := time.Now()
	for _, e := range entries {
		if err = ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		if strings.HasPrefix(name, ".blob-") {
			// All writers stage under this same lock, so any staging file seen
			// here belongs to an interrupted writer and can be discarded.
			st, err := e.Info()
			if err != nil {
				return err
			}
			if err := regular(st); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if err := os.Remove(filepath.Join(c.Root, name)); err != nil {
				return err
			}
			continue
		}
		kind, rest, ok := strings.Cut(name, "-")
		if !ok || !strings.HasSuffix(rest, ".blob") {
			continue
		}
		if _, err := blobName(kind, strings.TrimSuffix(rest, ".blob")); err != nil {
			continue
		}
		st, err := e.Info()
		if err != nil {
			return err
		}
		if err = regular(st); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if st.Size() > MaxBytes {
			return fmt.Errorf("%s: blob exceeds %d bytes", name, MaxBytes)
		}
		if name == replacing {
			// Free the old disposable blob before staging its replacement;
			// excluding it from accounting while it exists exceeds the cap.
			if err := os.Remove(filepath.Join(c.Root, name)); err != nil {
				return err
			}
			continue
		}
		files = append(files, entry{filepath.Join(c.Root, name), st.Size(), st.ModTime()})
		total += st.Size()
	}
	// Idle expiry does not need ordering. Only sort when space eviction must
	// choose the least-recently-used entries.
	if total > budget {
		sort.Slice(files, func(i, j int) bool {
			if files[i].at.Equal(files[j].at) {
				return files[i].path < files[j].path
			}
			return files[i].at.Before(files[j].at)
		})
	}
	for _, f := range files {
		if err = ctx.Err(); err != nil {
			return err
		}
		if total <= budget && now.Sub(f.at) <= c.TTL {
			continue
		}
		if err = os.Remove(f.path); err != nil {
			return err
		}
		total -= f.size
	}
	c.lastPrune.Store(now.UnixNano())
	return nil
}
