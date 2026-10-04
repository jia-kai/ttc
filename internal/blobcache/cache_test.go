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

func key(n int) string                         { return fmt.Sprintf("%064x", n) }
func path(c *Cache, kind string, n int) string { return filepath.Join(c.Root, kind+"-"+key(n)+".blob") }
func openCache(t *testing.T) *Cache {
	t.Helper()
	c, err := New(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func put(t *testing.T, c *Cache, kind string, n int, data string) {
	t.Helper()
	if err := c.Put(context.Background(), kind, key(n), []byte(data)); err != nil {
		t.Fatal(err)
	}
}
func miss(t *testing.T, c *Cache, kind string, n int) {
	t.Helper()
	if _, err := c.Get(context.Background(), kind, key(n)); !errors.Is(err, ErrMiss) {
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
func blobBytes(t *testing.T, c *Cache) int64 {
	t.Helper()
	entries, err := os.ReadDir(c.Root)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".blob") {
			continue
		}
		st, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += st.Size()
	}
	return total
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
	put(t, c, "original", 1, "bytes")
	for p, mode := range map[string]os.FileMode{c.Root: 0700, filepath.Join(c.Root, ".lock"): 0600, path(c, "original", 1): 0600} {
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
	link := filepath.Join(t.TempDir(), "symlink")
	if err := os.Symlink(c.Root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := New(link); err == nil {
		t.Fatal("accepted symlink root")
	}
}

func TestMixedBudgetTouchTTLAndSelfEviction(t *testing.T) {
	c := openCache(t)
	c.Limit = 8
	put(t, c, "original", 1, "aaaa")
	put(t, c, "render", 1, "bbbb") // Same key, separate namespace.
	age(t, path(c, "original", 1), 2*time.Hour)
	age(t, path(c, "render", 1), time.Hour)
	b, err := c.Get(context.Background(), "original", key(1))
	if err != nil || string(b) != "aaaa" {
		t.Fatal(string(b), err)
	}
	put(t, c, "render", 2, "cccc")
	miss(t, c, "render", 1)
	if b, err = c.Get(context.Background(), "original", key(1)); err != nil || string(b) != "aaaa" {
		t.Fatal("hit did not touch LRU", err)
	}
	if total := blobBytes(t, c); total != 8 {
		t.Fatal(total)
	}
	age(t, path(c, "original", 1), 31*24*time.Hour)
	miss(t, c, "original", 1) // Requested TTL is enforced even after a recent prune.
	age(t, path(c, "render", 2), 31*24*time.Hour)
	c.lastPrune.Store(0)
	miss(t, c, "render", 999) // A missing read also opportunistically prunes idle blobs.
	miss(t, c, "render", 2)
	put(t, c, "original", 3, "123456789") // Too large for this configured budget.
	miss(t, c, "original", 3)
	if total := blobBytes(t, c); total != 0 {
		t.Fatal("self eviction", total)
	}
}

func TestReplacementAndExplicitPrune(t *testing.T) {
	c := openCache(t)
	c.Limit = 8
	put(t, c, "original", 1, "1234")
	put(t, c, "render", 2, "5678")
	put(t, c, "original", 1, "abcd") // Replacing does not double-count the target.
	if total := blobBytes(t, c); total != 8 {
		t.Fatal(total)
	}
	if _, err := c.Get(context.Background(), "render", key(2)); err != nil {
		t.Fatal("replacement needlessly evicted", err)
	}
	c.Limit = 3
	if err := c.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if total := blobBytes(t, c); total > c.Limit {
		t.Fatal(total)
	}
}

func TestReplacementHardCapIncludesStaging(t *testing.T) {
	c := openCache(t)
	data := bytes.Repeat([]byte("x"), 8<<20)
	c.Limit = int64(len(data))
	if err := c.Put(context.Background(), "original", key(1), data); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for range 5 {
			if err := c.Put(context.Background(), "original", key(1), data); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	var peak int64
	for {
		entries, err := os.ReadDir(c.Root)
		if err != nil {
			t.Fatal(err)
		}
		var total int64
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".blob") && !strings.HasPrefix(e.Name(), ".blob-") {
				continue
			}
			st, err := e.Info()
			if os.IsNotExist(err) { // The writer can rename/remove between list and stat.
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			total += st.Size()
		}
		peak = max(peak, total)
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			if peak > c.Limit {
				t.Fatal("replacement staging exceeded hard cap", peak, c.Limit)
			}
			return
		default:
			runtime.Gosched()
		}
	}
}

func TestOrphanStagingIsDiscardedBeforePublicationAndPruning(t *testing.T) {
	c := openCache(t)
	c.Limit = 8
	orphan := filepath.Join(c.Root, ".blob-interrupted")
	if err := os.WriteFile(orphan, []byte("12345678"), 0600); err != nil {
		t.Fatal(err)
	}
	put(t, c, "original", 1, "abcdefgh")
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("publication retained orphan staging", err)
	}
	if err := os.WriteFile(orphan, []byte("12345678"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("prune retained orphan staging", err)
	}
	if total := blobBytes(t, c); total != 8 {
		t.Fatal("orphan cleanup removed valid blob", total)
	}
}

func TestMissingStorageIsNotCacheMiss(t *testing.T) {
	c := openCache(t)
	miss(t, c, "original", 1)
	if err := os.RemoveAll(c.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), "original", key(1)); err == nil || errors.Is(err, ErrMiss) {
		t.Fatal("missing cache storage was classified as a blob miss", err)
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
				t.Fatal("first-use operation did not respect cancellation", err)
			}
		case <-time.After(time.Second):
			t.Fatal("initialization blocked on filesystem lock")
		}
	}
}

func TestCorruptAndUnsafeEntriesDoNotBlock(t *testing.T) {
	for _, kind := range []string{"fifo", "symlink", "directory", "oversized", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			c := openCache(t)
			p := path(c, "original", 1)
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
				if err = f.Truncate(MaxBytes + 1); err != nil {
					t.Fatal(err)
				}
				f.Close()
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := c.Get(ctx, "original", key(1)); done <- err }()
			select {
			case err := <-done:
				if err == nil || errors.Is(err, ErrMiss) {
					t.Fatal("unsafe hit was accepted or treated as miss", err)
				}
			case <-ctx.Done():
				t.Fatal("unsafe descriptor blocked")
			}
			if err := c.Prune(ctx); err == nil {
				t.Fatal("prune accepted unsafe blob")
			}
		})
	}
	c := openCache(t)
	if err := c.Put(context.Background(), "original", key(1), make([]byte, MaxBytes+1)); err == nil {
		t.Fatal("accepted oversized bytes")
	}
}

func TestUnsafeLockMetadataIsRejected(t *testing.T) {
	for _, kind := range []string{"fifo", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			lock := filepath.Join(root, ".lock")
			var err error
			if kind == "fifo" {
				err = syscall.Mkfifo(lock, 0600)
			} else {
				err = os.Symlink(filepath.Join(root, "missing"), lock)
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := New(root); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("accepted unsafe lock metadata")
				}
			case <-time.After(time.Second):
				t.Fatal("blocked on unsafe lock metadata")
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
		func(ctx context.Context) error { return c.Put(ctx, "render", key(1), []byte("bytes")) },
		c.Prune,
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

func TestBoundedReadChecksCancellation(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "bytes")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = f.Truncate(MaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = readBounded(ctx, f, MaxBytes+1); !errors.Is(err, context.Canceled) {
		t.Fatal("read ignored cancellation", err)
	}
	// The descriptor can grow after its initial stat; the reader still enforces
	// the encoded limit independently of that metadata.
	if _, err = readBounded(context.Background(), f, MaxBytes); err == nil {
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
	got, err := readBounded(context.Background(), f, 1)
	if err != nil || string(got) != "growing bytes" {
		t.Fatal("initial size hint truncated a growing descriptor", string(got), err)
	}
}

func TestIndependentObjectsAndProcesses(t *testing.T) {
	// The subprocess runs the same bounded writer as independently opened objects.
	if root := os.Getenv("TTC_BLOBCACHE_TEST_ROOT"); root != "" {
		c, err := New(root)
		if err != nil {
			t.Fatal(err)
		}
		c.Limit = 128
		for n := range 60 {
			if err := c.Put(context.Background(), "original", key(n+1000), bytes.Repeat([]byte("z"), 16)); err != nil {
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
				total := blobBytes(t, object)
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
	if total := blobBytes(t, c); total > c.Limit {
		t.Fatal(total)
	}
	entries, err := os.ReadDir(c.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".blob-") {
			t.Fatal("leaked temporary file", e.Name())
		}
	}
}
