package blobcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func key(n int) string { return digest([]byte(fmt.Sprintf("key %d", n))) }
func openCache(t *testing.T) *Cache {
	t.Helper()
	c, err := New(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func put(t *testing.T, c *Cache, kind, k string, data []byte) {
	t.Helper()
	if err := c.Put(context.Background(), kind, k, data); err != nil {
		t.Fatal(err)
	}
}
func hit(t *testing.T, c *Cache, kind, k string, data []byte) {
	t.Helper()
	got, err := c.Get(context.Background(), kind, k)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("hit %s %s: %q, %v", kind, k, got, err)
	}
}
func miss(t *testing.T, c *Cache, kind, k string) {
	t.Helper()
	if _, err := c.Get(context.Background(), kind, k); !errors.Is(err, ErrMiss) {
		t.Fatal("expected miss", err)
	}
}
func age(t *testing.T, p string, d time.Duration) {
	t.Helper()
	at := time.Now().Add(-d)
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}
func totalBytes(t *testing.T, c *Cache) int64 {
	t.Helper()
	entries, err := os.ReadDir(c.Root)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, e := range entries {
		st, err := e.Info()
		if os.IsNotExist(err) {
			continue
		} // Concurrent rename/removal.
		if err != nil {
			t.Fatal(err)
		}
		total += st.Size()
	}
	return total
}
func stat(t *testing.T, c *Cache, name string) os.FileInfo {
	t.Helper()
	st, err := os.Stat(filepath.Join(c.Root, name))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestDefaultPrivacyAndValidation(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if c.Root != filepath.Join(os.Getenv("XDG_CACHE_HOME"), "ttc", "assets") || c.Limit != 4<<30 || c.TTL != 30*24*time.Hour {
		t.Fatalf("wrong defaults: %+v", c)
	}
	again, err := Default()
	if err != nil || again != c {
		t.Fatal("default was reopened", err)
	}
	data := []byte("bytes")
	put(t, c, "original", digest(data), data)
	put(t, c, "render", key(1), data)
	for p, mode := range map[string]os.FileMode{c.Root: 0700, filepath.Join(c.Root, ".lock"): 0600, filepath.Join(c.Root, blobName(digest(data))): 0600, filepath.Join(c.Root, refName(key(1))): 0600} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != mode {
			t.Fatal("private permissions", p, st, err)
		}
	}
	if _, err := New("relative"); err == nil {
		t.Fatal("accepted relative root")
	}
	t.Setenv("XDG_CACHE_HOME", "relative")
	if _, err := Default(); err == nil {
		t.Fatal("accepted relative XDG root")
	}
	for _, tc := range []struct{ kind, key string }{{"other", key(1)}, {"original", "../escape"}, {"render", strings.Repeat("A", 64)}} {
		if _, err := c.Get(context.Background(), tc.kind, tc.key); err == nil {
			t.Fatal("accepted invalid name", tc)
		}
		if err := c.Put(context.Background(), tc.kind, tc.key, nil); err == nil {
			t.Fatal("accepted invalid name", tc)
		}
	}
	if err := c.Put(context.Background(), "original", key(1), data); err == nil {
		t.Fatal("accepted non-content original key")
	}
	link := filepath.Join(t.TempDir(), "symlink")
	if err := os.Symlink(c.Root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := New(link); err == nil {
		t.Fatal("accepted symlink root")
	}
}

func TestSharedContentAddressAndDuplicatePutNeverRewrites(t *testing.T) {
	c := openCache(t)
	data := []byte("identical binary bytes")
	sha := digest(data)
	put(t, c, "original", sha, data)
	before := stat(t, c, blobName(sha))
	age(t, filepath.Join(c.Root, blobName(sha)), time.Hour)
	for n := range 3 {
		put(t, c, "render", key(n), data)
	}
	put(t, c, "original", sha, data)
	refBefore := stat(t, c, refName(key(0)))
	put(t, c, "render", key(0), data)
	after := stat(t, c, blobName(sha))
	if !os.SameFile(before, after) || !os.SameFile(refBefore, stat(t, c, refName(key(0)))) {
		t.Fatal("duplicate Put rewrote blob/reference")
	}
	if time.Since(after.ModTime()) > time.Minute {
		t.Fatal("duplicate Put did not touch blob")
	}
	if totalBytes(t, c) != int64(len(data)+3*64) {
		t.Fatal("content not physically deduplicated", totalBytes(t, c))
	}
	hit(t, c, "original", sha, data)
	for n := range 3 {
		hit(t, c, "render", key(n), data)
	}
}

func TestSharedBudgetIncludesMetadataAndTTL(t *testing.T) {
	c := openCache(t)
	c.Limit = 132 // Two four-byte blobs and two 64-byte recipe references.
	a, b, d := []byte("aaaa"), []byte("bbbb"), []byte("cccc")
	put(t, c, "original", digest(a), a)
	put(t, c, "render", key(1), b)
	age(t, filepath.Join(c.Root, blobName(digest(a))), 2*time.Hour)
	age(t, filepath.Join(c.Root, blobName(digest(b))), time.Hour)
	age(t, filepath.Join(c.Root, refName(key(1))), time.Hour)
	hit(t, c, "original", digest(a), a)
	put(t, c, "render", key(2), d)
	miss(t, c, "render", key(1))
	miss(t, c, "original", digest(b))
	hit(t, c, "original", digest(a), a)
	if total := totalBytes(t, c); total > c.Limit {
		t.Fatal("metadata escaped budget", total)
	}
	age(t, filepath.Join(c.Root, blobName(digest(d))), 31*24*time.Hour)
	miss(t, c, "render", key(2)) // Expired blob removes even recently touched references.
	if _, err := os.Stat(filepath.Join(c.Root, refName(key(2)))); !os.IsNotExist(err) {
		t.Fatal("expired blob retained reference", err)
	}
	age(t, filepath.Join(c.Root, blobName(digest(a))), 31*24*time.Hour)
	c.lastPrune.Store(0)
	miss(t, c, "render", key(999))
	miss(t, c, "original", digest(a))
	if totalBytes(t, c) != 0 {
		t.Fatal("TTL did not remove all data")
	}
	c.Limit = 3
	put(t, c, "original", digest(a), a)
	miss(t, c, "original", digest(a))
	c.Limit = 67 // Blob fits, blob + recipe does not.
	put(t, c, "render", key(1), a)
	miss(t, c, "render", key(1))
}

func TestDuplicatePutSkipsScanButStillPrunesPeriodically(t *testing.T) {
	for _, kind := range []string{"original", "render"} {
		t.Run(kind, func(t *testing.T) {
			c := openCache(t)
			data := []byte("duplicate")
			k := digest(data)
			if kind == "render" {
				k = key(1)
			}
			put(t, c, kind, k, data)
			before := stat(t, c, blobName(digest(data)))
			age(t, filepath.Join(c.Root, blobName(digest(data))), time.Hour)
			if kind == "render" {
				age(t, filepath.Join(c.Root, refName(k)), time.Hour)
			}
			// An unrelated interrupted stage reveals whether a full scan ran.
			stage := filepath.Join(c.Root, ".stage-interrupted")
			if err := os.WriteFile(stage, []byte("old staging"), 0600); err != nil {
				t.Fatal(err)
			}
			recent := time.Now().Add(-time.Second).UnixNano()
			c.lastPrune.Store(recent)
			put(t, c, kind, k, data)
			if c.lastPrune.Load() != recent {
				t.Fatal("duplicate Put scanned the directory")
			}
			if _, err := os.Stat(stage); err != nil {
				t.Fatal("duplicate Put scanned unrelated files", err)
			}
			after := stat(t, c, blobName(digest(data)))
			if !os.SameFile(before, after) || time.Since(after.ModTime()) > time.Minute {
				t.Fatal("duplicate Put failed to touch the existing blob")
			}
			if kind == "render" && time.Since(stat(t, c, refName(k)).ModTime()) > time.Minute {
				t.Fatal("duplicate Put failed to touch the recipe")
			}
			c.lastPrune.Store(time.Now().Add(-2 * time.Minute).UnixNano())
			put(t, c, kind, k, data)
			if _, err := os.Stat(stage); !os.IsNotExist(err) {
				t.Fatal("duplicate Put skipped periodic pruning", err)
			}
		})
	}
}

func TestRetentionPolicyChangesForceDuplicatePutScan(t *testing.T) {
	for _, policy := range []string{"limit", "ttl"} {
		for _, kind := range []string{"original", "render"} {
			t.Run(policy+"/"+kind, func(t *testing.T) {
				c := openCache(t)
				a, b := []byte("keep"), []byte("evict")
				k := digest(a)
				if kind == "render" {
					k = key(1)
				}
				put(t, c, kind, k, a)
				put(t, c, "original", digest(b), b)
				age(t, filepath.Join(c.Root, blobName(digest(b))), 2*time.Hour)
				if policy == "limit" {
					c.Limit = int64(len(a))
					if kind == "render" {
						c.Limit += 64
					}
				} else {
					c.TTL = time.Hour
				}
				put(t, c, kind, k, a)
				if _, err := os.Stat(filepath.Join(c.Root, blobName(digest(b)))); !os.IsNotExist(err) {
					t.Fatal("policy change did not force eviction", err)
				}
				hit(t, c, kind, k, a)
				if totalBytes(t, c) > c.Limit {
					t.Fatal("duplicate Put exceeded changed budget")
				}
			})
		}
	}
}

func TestGrowingPutScansEvenAfterRecentPrune(t *testing.T) {
	c := openCache(t)
	c.Limit = 68
	data := []byte("data")
	put(t, c, "render", key(1), data)
	stage := filepath.Join(c.Root, ".stage-interrupted")
	if err := os.WriteFile(stage, []byte("orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	// Sharing the blob still grows recipe metadata and must reserve capacity.
	put(t, c, "render", key(2), data)
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatal("growing write skipped pruning", err)
	}
	if totalBytes(t, c) != c.Limit {
		t.Fatal("growing write exceeded metadata budget", totalBytes(t, c))
	}
	miss(t, c, "render", key(1))
	hit(t, c, "render", key(2), data)
}

func TestRecipeIdleTTLIsIndependentOfSharedBlob(t *testing.T) {
	c := openCache(t)
	data := []byte("shared")
	for n := range 2 {
		put(t, c, "render", key(n), data)
	}
	age(t, filepath.Join(c.Root, refName(key(0))), 31*24*time.Hour)
	hit(t, c, "render", key(1), data)
	miss(t, c, "render", key(0))
	hit(t, c, "original", digest(data), data)
}

func TestReplacementAndExplicitPrune(t *testing.T) {
	c := openCache(t)
	c.Limit = 136
	a, b := []byte("1234"), []byte("5678")
	put(t, c, "render", key(1), a)
	put(t, c, "render", key(2), a)
	put(t, c, "render", key(1), b)
	hit(t, c, "render", key(1), b)
	hit(t, c, "render", key(2), a)
	if totalBytes(t, c) != 136 {
		t.Fatal("wrong replacement accounting", totalBytes(t, c))
	}
	c.Limit = 3
	if err := c.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if totalBytes(t, c) > c.Limit {
		t.Fatal("prune exceeded capacity")
	}
	miss(t, c, "render", key(1))
	miss(t, c, "render", key(2))
}

func TestHardCapIncludesStagingAndDuplicatePutDoesNotStage(t *testing.T) {
	c := openCache(t)
	data := bytes.Repeat([]byte("x"), 2<<20)
	c.Limit = int64(len(data) + 128)
	put(t, c, "original", digest(data), data)
	before := stat(t, c, blobName(digest(data)))
	done := make(chan error, 1)
	go func() {
		for n := range 5 {
			if err := c.Put(context.Background(), "render", key(n), data); err != nil {
				done <- err
				return
			}
		}
		// This stages a different blob, forcing eviction of the shared old one.
		other := bytes.Repeat([]byte("y"), len(data))
		done <- c.Put(context.Background(), "render", key(99), other)
	}()
	for {
		if total := totalBytes(t, c); total > c.Limit {
			t.Fatal("staging or metadata exceeded hard cap", total, c.Limit)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(c.Root, blobName(digest(data)))); !os.IsNotExist(err) {
				t.Fatal("old content not evicted", err, before)
			}
			return
		default:
			runtime.Gosched()
		}
	}
}

func TestOrphanStagingAndOldFormatAreDiscarded(t *testing.T) {
	c := openCache(t)
	c.Limit = 8
	for _, name := range []string{".stage-interrupted", ".blob-interrupted", "original-" + key(1) + ".blob", "render-" + key(1) + ".blob", "unknown"} {
		if err := os.WriteFile(filepath.Join(c.Root, name), []byte("12345678"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	data := []byte("abcdefgh")
	put(t, c, "original", digest(data), data)
	if totalBytes(t, c) != 8 {
		t.Fatal("obsolete cache data retained")
	}
	if err := c.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	hit(t, c, "original", digest(data), data)
}

func TestDanglingReferenceAndCorruption(t *testing.T) {
	c := openCache(t)
	data := []byte("valid")
	put(t, c, "render", key(1), data)
	if err := os.Remove(filepath.Join(c.Root, blobName(digest(data)))); err != nil {
		t.Fatal(err)
	}
	// A Put repairs a reference whose blob was removed externally.
	put(t, c, "render", key(1), data)
	hit(t, c, "render", key(1), data)
	if err := os.WriteFile(filepath.Join(c.Root, blobName(digest(data))), []byte("wrong"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"original", "render"} {
		k := digest(data)
		if kind == "render" {
			k = key(1)
		}
		if _, err := c.Get(context.Background(), kind, k); err == nil || errors.Is(err, ErrMiss) {
			t.Fatal("corrupt bytes were not a loud error", err)
		}
		if err := c.Put(context.Background(), kind, k, data); err == nil {
			t.Fatal("duplicate Put silently repaired corrupt bytes")
		}
	}
	if err := os.WriteFile(filepath.Join(c.Root, refName(key(1))), bytes.Repeat([]byte("q"), 64), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.Prune(context.Background()); err == nil {
		t.Fatal("corrupt reference accepted")
	}
}

func TestMissingStorageIsNotCacheMiss(t *testing.T) {
	c := openCache(t)
	miss(t, c, "original", key(1))
	if err := os.RemoveAll(c.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), "original", key(1)); err == nil || errors.Is(err, ErrMiss) {
		t.Fatal("missing storage classified as miss", err)
	}
}

func TestInitializationDoesNotWaitForHeldLock(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	c, err := New(filepath.Join(os.Getenv("XDG_CACHE_HOME"), "ttc", "assets"))
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := c.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for _, open := range []func() (*Cache, error){func() (*Cache, error) { return New(c.Root) }, Default} {
		done := make(chan error, 1)
		go func() {
			cache, err := open()
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				_, err = cache.Get(ctx, "original", key(1))
				cancel()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("first operation ignored cancellation", err)
			}
		case <-time.After(time.Second):
			t.Fatal("initialization blocked on lock")
		}
	}
}

func TestCorruptAndUnsafeEntriesDoNotBlock(t *testing.T) {
	for _, namespace := range []string{"blob", "reference", "obsolete"} {
		for _, kind := range []string{"fifo", "symlink", "directory", "oversized", "hardlink"} {
			t.Run(namespace+"/"+kind, func(t *testing.T) {
				c := openCache(t)
				name, readKind := blobName(key(1)), "original"
				if namespace == "reference" {
					name, readKind = refName(key(1)), "render"
				}
				if namespace == "obsolete" {
					name = "original-" + key(1) + ".blob"
				}
				p := filepath.Join(c.Root, name)
				switch kind {
				case "fifo":
					if err := syscall.Mkfifo(p, 0600); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(p, 0700); err != nil {
						t.Fatal(err)
					}
				case "symlink", "hardlink":
					target := filepath.Join(t.TempDir(), "target")
					if err := os.WriteFile(target, []byte("unsafe"), 0600); err != nil {
						t.Fatal(err)
					}
					var err error
					if kind == "symlink" {
						err = os.Symlink(target, p)
					} else {
						err = os.Link(target, p)
					}
					if err != nil {
						t.Fatal(err)
					}
				case "oversized":
					f, err := os.Create(p)
					if err != nil {
						t.Fatal(err)
					}
					if err := f.Truncate(MaxBytes + 1); err != nil {
						t.Fatal(err)
					}
					f.Close()
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_, err := c.Get(ctx, readKind, key(1))
				// Disposable obsolete regular bytes are removed, not imported.
				if namespace == "obsolete" && kind == "oversized" {
					if !errors.Is(err, ErrMiss) {
						t.Fatal(err)
					}
					return
				}
				if err == nil || errors.Is(err, ErrMiss) {
					t.Fatal("unsafe entry accepted or treated as miss", err)
				}
				if err := c.Prune(ctx); err == nil {
					t.Fatal("prune accepted unsafe entry")
				}
			})
		}
	}
	c := openCache(t)
	if err := c.Put(context.Background(), "original", key(1), make([]byte, MaxBytes+1)); err == nil {
		t.Fatal("accepted oversized bytes")
	}
}

func TestUnsafeLockMetadataIsRejected(t *testing.T) {
	for _, kind := range []string{"fifo", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			p := filepath.Join(root, ".lock")
			var err error
			if kind == "fifo" {
				err = syscall.Mkfifo(p, 0600)
			} else if kind == "symlink" {
				err = os.Symlink(filepath.Join(root, "missing"), p)
			} else {
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, nil, 0600); err != nil {
					t.Fatal(err)
				}
				err = os.Link(target, p)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := New(root); err == nil {
				t.Fatal("accepted unsafe lock")
			}
		})
	}
}

func TestCancellationWaitingForLockAndNoTemporaryFiles(t *testing.T) {
	c := openCache(t)
	unlock, err := c.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for _, op := range []func(context.Context) error{
		func(ctx context.Context) error { _, err := c.Get(ctx, "original", key(1)); return err },
		func(ctx context.Context) error { return c.Put(ctx, "render", key(1), []byte("bytes")) }, c.Prune,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		err := op(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("lock wait ignored cancellation", err)
		}
	}
	entries, err := os.ReadDir(c.Root)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".lock" {
		t.Fatal("partial files", entries, err)
	}
}

func TestBoundedReadChecksCancellationAndGrowth(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "bytes")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(MaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readBounded(ctx, f, MaxBytes+1, MaxBytes); !errors.Is(err, context.Canceled) {
		t.Fatal("read ignored cancellation", err)
	}
	if _, err := readBounded(context.Background(), f, MaxBytes, MaxBytes); err == nil {
		t.Fatal("read accepted oversized descriptor")
	}
}

func TestBoundedReadAcceptsGrowthBeyondInitialSize(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "growing")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("growing bytes"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	got, err := readBounded(context.Background(), f, 1, MaxBytes)
	if err != nil || string(got) != "growing bytes" {
		t.Fatal("size hint truncated growth", string(got), err)
	}
}

func TestIndependentObjectsAndProcesses(t *testing.T) {
	if root := os.Getenv("TTC_BLOBCACHE_TEST_ROOT"); root != "" {
		c, err := New(root)
		if err != nil {
			t.Fatal(err)
		}
		c.Limit = 128
		for n := range 60 {
			data := []byte(fmt.Sprintf("original %d", n))
			if err := c.Put(context.Background(), "original", digest(data), data); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	c := openCache(t)
	c.Limit = 128
	other, err := New(c.Root)
	if err != nil {
		t.Fatal(err)
	}
	other.Limit = c.Limit
	cmd := exec.Command(os.Args[0], "-test.run=^TestIndependentObjectsAndProcesses$")
	cmd.Env = append(os.Environ(), "TTC_BLOBCACHE_TEST_ROOT="+c.Root)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			object := []*Cache{c, other}[worker%2]
			for n := range 15 {
				if err := object.Put(context.Background(), "render", key(worker*100+n), bytes.Repeat([]byte("x"), 16)); err != nil {
					t.Error(err)
					return
				}
				unlock, err := object.lock(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				total := totalBytes(t, object)
				unlock()
				if total > object.Limit {
					t.Error("exceeded hard budget", total)
				}
			}
		}()
	}
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err, output.String())
	}
	if totalBytes(t, c) > c.Limit {
		t.Fatal("budget exceeded")
	}
	entries, err := os.ReadDir(c.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".stage-") {
			t.Fatal("leaked temporary file", e.Name())
		}
	}
}
