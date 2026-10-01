package assets

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestRendererCaptureBoundsCopyFastPaths(t *testing.T) {
	b := limitedBuffer{limit: 32}
	// Hide Reader.WriteTo so io.Copy would use a promoted Buffer.ReadFrom.
	_, err := io.Copy(&b, struct{ io.Reader }{strings.NewReader(strings.Repeat("x", 4096))})
	if err == nil || b.buffer.Len() != 32 {
		t.Fatal("renderer capture escaped its budget", b.buffer.Len(), err)
	}
}

func TestReadRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.pipe")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := Read(path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular PNG") {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		// Release a regressed blocking open before reporting the failure.
		f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
		if err == nil {
			<-done
			f.Close()
		}
		t.Fatal("image read blocked on FIFO")
	}
}

func TestCacheReusePruneConcurrencyAndPrivacy(t *testing.T) {
	c, err := New(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	calls := 0
	create := func(context.Context) (image.Image, error) {
		calls++
		return image.NewNRGBA(image.Rect(0, 0, 20, 10)), nil
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Get(ctx, Key("same"), create); err != nil {
				t.Error(err)
			}
			if err := c.Prune(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatal("duplicate render", calls)
	}
	p := filepath.Join(c.Root, Key("same")+".png")
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
	old := time.Now().Add(-31 * 24 * time.Hour)
	if err = os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if err = c.Prune(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("TTL not pruned", err)
	}
	c.Limit = 150
	for i := range 5 {
		if _, err = c.Get(ctx, Key(fmt.Sprint(i)), create); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(c.Root)
	var total int64
	for _, v := range entries {
		st, _ := v.Info()
		total += st.Size()
	}
	if total > c.Limit {
		t.Fatal("cache limit", total)
	}
}

func TestCacheHitRejectsOversizedFileBeforeRead(t *testing.T) {
	c, err := New(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	key := Key("oversized")
	f, err := os.Create(filepath.Join(c.Root, key+".png"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(MaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err = c.Get(context.Background(), key, func(context.Context) (image.Image, error) { t.Fatal("invalid hit treated as miss"); return nil, nil }); err == nil {
		t.Fatal("accepted oversized cache hit")
	}
}

func TestWarmRendererIgnoresNodeHooksAndWorkspaceModules(t *testing.T) {
	isolatedMathCache(t)
	dir := t.TempDir()
	hook := filepath.Join(dir, "hook.cjs")
	if err := os.WriteFile(hook, []byte("throw new Error('external Node hook executed');\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_OPTIONS", "--require="+hook)
	t.Setenv("NODE_PATH", dir)
	shadow := filepath.Join(dir, "node_modules", "@mathjax", "src")
	if err := os.MkdirAll(shadow, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shadow, "index.js"), []byte("throw new Error('workspace module executed');\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	r, err := NewMathRenderer(context.Background())
	if err != nil {
		t.Fatal("workspace module shadowed cached backend", err)
	}
	defer r.Close()
	m, err := r.Render(context.Background(), `\frac{1}{2}`, 16)
	if err != nil {
		t.Fatal("external Node configuration escaped local MathJax", err)
	}
	visible, transparent := false, false
	for y := range m.Bounds().Dy() {
		for x := range m.Bounds().Dx() {
			_, _, _, a := m.At(x, y).RGBA()
			visible = visible || a > 0
			transparent = transparent || a == 0
		}
	}
	if !visible || !transparent {
		t.Fatal("formula lacks visible transparent glyphs")
	}
}
func TestCacheCreationAndWriteErrorsNotHidden(t *testing.T) {
	c, err := New(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = c.Get(ctx, "../../escape", nil); err == nil {
		t.Fatal("accepted invalid key")
	}
	if _, err = c.Get(ctx, Key("error"), func(context.Context) (image.Image, error) { return nil, fmt.Errorf("broken backend") }); err == nil {
		t.Fatal("masked creation error")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = c.Get(canceled, Key("canceled"), nil); err == nil {
		t.Fatal("ignored cancellation")
	}
	if err = os.Remove(c.Root); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(c.Root, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Get(ctx, Key("write"), func(context.Context) (image.Image, error) { return image.NewNRGBA(image.Rect(0, 0, 1, 1)), nil }); err == nil {
		t.Fatal("masked storage failure")
	}
}
func TestDecodeAndAspectBounds(t *testing.T) {
	m := image.NewNRGBA(image.Rect(0, 0, 320, 200))
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(b.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(make([]byte, MaxBytes+1)); err == nil {
		t.Fatal("unbounded encoded bytes")
	}
	// DecodeConfig sees oversized dimensions without needing decoded pixels.
	oversized := append([]byte(nil), b.Bytes()...)
	binary.BigEndian.PutUint32(oversized[16:20], MaxPixels+1)
	binary.BigEndian.PutUint32(oversized[20:24], 1)
	binary.BigEndian.PutUint32(oversized[29:33], crc32.ChecksumIEEE(oversized[12:29]))
	if _, err := Decode(oversized); err == nil || !strings.Contains(err.Error(), "pixels") {
		t.Fatal("unbounded pixel allocation", err)
	}
	resized := Resize(m, 80, 100)
	if resized.Bounds().Dx() != 80 || resized.Bounds().Dy() != 50 {
		t.Fatal(resized.Bounds())
	}
}
func TestMathInitializationCancellationWithoutHostDependencies(t *testing.T) {
	// Core tests do not require optional host renderers. A canceled context must
	// stop before an unavailable backend can become a persistent render worker.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewMathRenderer(ctx); err == nil {
		t.Fatal("ignored cancellation")
	}
}
