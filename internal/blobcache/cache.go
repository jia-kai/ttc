// Package blobcache stores disposable binary bytes in a shared filesystem
// content-addressable cache. Recipe references and blobs share one idle TTL and
// least-recently-used budget; durable history is outside this cache.
package blobcache

import (
	"context"
	"crypto/sha256"
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

// MaxBytes bounds each binary blob, including reads of externally changed files.
const MaxBytes = 32 << 20

// ErrMiss identifies an absent or expired entry, not a cache-storage failure.
var ErrMiss = errors.New("blob cache miss")

// Cache coordinates operations with short-lived cancelable filesystem locks.
// Entries are never pinned. Configure Limit and TTL before concurrent use.
type Cache struct {
	Root       string        // Absolute directory containing private blobs, recipe references and .lock.
	Limit      int64         // Total file-content bytes, including references/staging; defaults to 4 GiB. The empty lock file and filesystem allocation overhead are excluded.
	TTL        time.Duration // Idle retention of blobs and individual recipe references; defaults to 30 days.
	lastPrune  atomic.Int64
	pruneLimit atomic.Int64 // Policy used by the last successful scan.
	pruneTTL   atomic.Int64
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

func validKey(key string) bool { return len(key) == 64 && strings.Trim(key, "0123456789abcdef") == "" }
func validate(kind, key string) error {
	if kind != "original" && kind != "render" {
		return fmt.Errorf("invalid blob kind %q", kind)
	}
	if !validKey(key) {
		return fmt.Errorf("invalid blob key")
	}
	return nil
}
func digest(data []byte) string  { return fmt.Sprintf("%x", sha256.Sum256(data)) }
func blobName(key string) string { return "blob-" + key + ".blob" }
func refName(key string) string  { return "render-" + key + ".ref" }

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

func (c *Cache) open(name string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(filepath.Join(c.Root, name), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err == nil {
		err = regular(st)
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, st, nil
}

// load validates bytes against their content address, or validates a reference's
// exact 64-byte digest. It never classifies corrupt contents as a cache miss.
func (c *Cache) load(ctx context.Context, name, expected string) ([]byte, os.FileInfo, error) {
	f, st, err := c.open(name)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	limit := int64(MaxBytes)
	if expected == "" {
		limit = 64
	}
	if st.Size() > limit {
		return nil, nil, fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	b, err := readBounded(ctx, f, st.Size(), int(limit))
	if err != nil {
		return nil, nil, err
	}
	if expected == "" {
		if !validKey(string(b)) {
			return nil, nil, fmt.Errorf("corrupt cache reference %s", name)
		}
	} else if digest(b) != expected {
		return nil, nil, fmt.Errorf("cache content checksum mismatch: %s", name)
	}
	return b, st, nil
}

func (c *Cache) touch(name string) error {
	f, _, err := c.open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	now := syscall.NsecToTimeval(time.Now().UnixNano())
	return syscall.Futimes(int(f.Fd()), []syscall.Timeval{now, now})
}

// Get returns checksum-verified bytes and touches idle/LRU time. Original keys
// are SHA256(bytes); render keys address recipe references. An absent or expired
// entry matches ErrMiss; corrupt, unsafe and oversized files fail loudly.
// Reads opportunistically prune the whole cache at most once per minute.
func (c *Cache) Get(ctx context.Context, kind, key string) ([]byte, error) {
	if err := validate(kind, key); err != nil {
		return nil, err
	}
	unlock, err := c.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	miss := func() ([]byte, error) {
		if err := c.maybePrune(ctx); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s %s", ErrMiss, kind, key)
	}
	ref := ""
	if kind == "render" {
		ref = refName(key)
		b, st, err := c.load(ctx, ref, "")
		if os.IsNotExist(err) {
			return miss()
		}
		if err != nil {
			return nil, err
		}
		if time.Since(st.ModTime()) > c.TTL {
			if err := os.Remove(filepath.Join(c.Root, ref)); err != nil {
				return nil, err
			}
			return miss()
		}
		key = string(b)
	}
	name := blobName(key)
	b, st, err := c.load(ctx, name, key)
	if os.IsNotExist(err) {
		if ref != "" {
			// A dangling reference is an ordinary eviction miss, but it and
			// its peers must not occupy the metadata budget indefinitely.
			if err := c.prune(ctx, c.Limit, "", ""); err != nil {
				return nil, err
			}
		}
		return miss()
	}
	if err != nil {
		return nil, err
	}
	if time.Since(st.ModTime()) > c.TTL {
		// Remove all references to the expired blob, even after a recent prune.
		if err := c.prune(ctx, c.Limit, "", ""); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: expired %s", ErrMiss, name)
	}
	if err := c.touch(name); err != nil {
		return nil, err
	}
	if ref != "" {
		if err := c.touch(ref); err != nil {
			return nil, err
		}
	}
	if err := c.maybePrune(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

func readBounded(ctx context.Context, f *os.File, size int64, limit int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	capacity := int(min(max(size, 0), int64(limit)))
	b := make([]byte, 0, capacity)
	buf := make([]byte, min(max(capacity+1, 4<<10), 64<<10))
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := f.Read(buf)
		if len(b)+n > limit {
			return nil, fmt.Errorf("blob exceeds %d bytes", limit)
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

// Put publishes one SHA256-addressed blob and, for renders, a 64-byte recipe
// reference. Original keys must equal SHA256(data). Existing identical blobs
// are verified and touched, never rewritten or staged, across both namespaces.
// Eviction reserves room for metadata and staging before writing. Cancellation
// can leave an old recipe absent or an unreferenced complete blob, never partial
// published bytes. An entry larger than Limit is not retained (success).
func (c *Cache) Put(ctx context.Context, kind, key string, data []byte) error {
	if err := validate(kind, key); err != nil {
		return err
	}
	if len(data) > MaxBytes {
		return fmt.Errorf("blob exceeds %d bytes", MaxBytes)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sha := digest(data)
	if kind == "original" && key != sha {
		return fmt.Errorf("original key does not match content SHA256")
	}
	unlock, err := c.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	name := blobName(sha)
	_, _, err = c.load(ctx, name, sha)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if exists {
		if err := c.touch(name); err != nil {
			return err
		}
	}
	ref, sameRef := "", false
	if kind == "render" {
		ref = refName(key)
		b, _, err := c.load(ctx, ref, "")
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			sameRef = string(b) == sha && exists
			if sameRef {
				if err := c.touch(ref); err != nil {
					return err
				}
			} else if err := os.Remove(filepath.Join(c.Root, ref)); err != nil {
				return err
			}
		}
	}
	required := int64(len(data))
	if ref != "" {
		required += 64
	}
	if required > c.Limit {
		return c.prune(ctx, c.Limit, "", "")
	}
	var reserve int64
	if !exists {
		reserve += int64(len(data))
	}
	if ref != "" && !sameRef {
		reserve += 64
	}
	if exists && (ref == "" || sameRef) {
		// Verified duplicates only refresh idle time. Growing writes below
		// still scan and reserve their full staging/metadata footprint.
		return c.maybePrune(ctx)
	}
	protectedRef := ""
	if sameRef {
		protectedRef = ref
	}
	if err := c.prune(ctx, c.Limit-reserve, sha, protectedRef); err != nil {
		return err
	}
	if !exists {
		if err := c.publish(ctx, name, data); err != nil {
			return err
		}
	}
	if ref != "" && !sameRef {
		return c.publish(ctx, ref, []byte(sha))
	}
	return ctx.Err()
}

func (c *Cache) publish(ctx context.Context, name string, data []byte) error {
	f, err := os.CreateTemp(c.Root, ".stage-*")
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
		n, err := f.Write(remaining[:min(len(remaining), 64<<10)])
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		remaining = remaining[n:]
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(c.Root, name))
}

// Prune enforces the shared budget and idle TTL under a cancelable lock. Stale
// references, interrupted staging and obsolete cache files are discarded; no
// migration is attempted. Unsafe files are errors, not silently removed.
func (c *Cache) Prune(ctx context.Context) error {
	unlock, err := c.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return c.prune(ctx, c.Limit, "", "")
}
func (c *Cache) maybePrune(ctx context.Context) error {
	if c.pruneLimit.Load() == c.Limit && c.pruneTTL.Load() == int64(c.TTL) && time.Since(time.Unix(0, c.lastPrune.Load())) < time.Minute {
		return ctx.Err()
	}
	return c.prune(ctx, c.Limit, "", "")
}

func (c *Cache) prune(ctx context.Context, budget int64, protected, protectedRef string) error {
	entries, err := os.ReadDir(c.Root)
	if err != nil {
		return err
	}
	type entry struct {
		name, sha string
		size      int64
		at        time.Time
		ref       bool
	}
	files := make(map[string]entry)
	refs := make(map[string][]string)
	var total int64
	now := time.Now()
	remove := func(name string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(c.Root, name)); err != nil {
			return err
		}
		if f, ok := files[name]; ok {
			total -= f.size
			delete(files, name)
		}
		return nil
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		st, err := e.Info()
		if err != nil {
			return err
		}
		if err := regular(st); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if name == ".lock" {
			if st.Size() != 0 {
				return fmt.Errorf("cache lock must be empty")
			}
			continue
		}
		f := entry{name: name, size: st.Size(), at: st.ModTime()}
		if strings.HasPrefix(name, "blob-") && strings.HasSuffix(name, ".blob") && validKey(strings.TrimSuffix(strings.TrimPrefix(name, "blob-"), ".blob")) {
			f.sha = strings.TrimSuffix(strings.TrimPrefix(name, "blob-"), ".blob")
			if st.Size() > MaxBytes {
				return fmt.Errorf("%s: blob exceeds %d bytes", name, MaxBytes)
			}
		} else if strings.HasPrefix(name, "render-") && strings.HasSuffix(name, ".ref") && validKey(strings.TrimSuffix(strings.TrimPrefix(name, "render-"), ".ref")) {
			b, _, err := c.load(ctx, name, "")
			if err != nil {
				return err
			}
			f.sha, f.ref = string(b), true
			refs[f.sha] = append(refs[f.sha], name)
		} else {
			// The directory is exclusively cache-owned. This also removes old
			// original-/render-*.blob files and interrupted staging, not copies.
			if err := remove(name); err != nil {
				return err
			}
			continue
		}
		files[name] = f
		total += f.size
	}
	removeBlob := func(f entry) error {
		// References disappear first, so cancellation cannot leave dangling
		// references to a blob removed by this operation.
		for _, name := range refs[f.sha] {
			if _, ok := files[name]; ok {
				if err := remove(name); err != nil {
					return err
				}
			}
		}
		return remove(f.name)
	}
	for _, f := range files {
		if f.ref {
			_, hasBlob := files[blobName(f.sha)]
			if f.name != protectedRef && (!hasBlob || now.Sub(f.at) > c.TTL) {
				if err := remove(f.name); err != nil {
					return err
				}
			}
		} else if f.sha != protected && now.Sub(f.at) > c.TTL {
			if err := removeBlob(f); err != nil {
				return err
			}
		}
	}
	if total > budget {
		ordered := make([]entry, 0, len(files))
		for _, f := range files {
			ordered = append(ordered, f)
		}
		sort.Slice(ordered, func(i, j int) bool {
			if ordered[i].at.Equal(ordered[j].at) {
				return ordered[i].name < ordered[j].name
			}
			return ordered[i].at.Before(ordered[j].at)
		})
		for _, f := range ordered {
			if total <= budget {
				break
			}
			if _, ok := files[f.name]; !ok {
				continue
			}
			if !f.ref && f.sha == protected {
				continue
			}
			if f.name == protectedRef {
				continue
			}
			if f.ref {
				err = remove(f.name)
			} else {
				err = removeBlob(f)
			}
			if err != nil {
				return err
			}
		}
	}
	if total > budget {
		return fmt.Errorf("cache cannot reserve space within total limit")
	}
	c.pruneLimit.Store(c.Limit)
	c.pruneTTL.Store(int64(c.TTL))
	c.lastPrune.Store(now.UnixNano())
	return ctx.Err()
}
