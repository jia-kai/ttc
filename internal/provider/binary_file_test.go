package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"ttc/internal/blobcache"
)

func fileBinaryFile(t *testing.T, data []byte) BinaryFile {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return BinaryFile{Path: path, SHA256: hex.EncodeToString(sum[:])}
}

func TestFileBinaryFileURLPreservesOriginalBytes(t *testing.T) {
	// Incompressible pixels exercise reads and transport payloads above 64 KiB.
	m := image.NewNRGBA(image.Rect(0, 0, 257, 257))
	state := uint32(1)
	for i := range m.Pix {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		m.Pix[i] = byte(state)
	}
	for _, format := range []string{"png", "jpeg", "gif"} {
		t.Run(format, func(t *testing.T) {
			var data bytes.Buffer
			var err error
			switch format {
			case "png":
				err = png.Encode(&data, m)
			case "jpeg":
				err = jpeg.Encode(&data, m, &jpeg.Options{Quality: 100})
			case "gif":
				frame := image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White})
				err = gif.EncodeAll(&data, &gif.GIF{Image: []*image.Paletted{frame, frame}, Delay: []int{1, 2}})
			}
			if err != nil {
				t.Fatal(err)
			}
			if format == "png" && data.Len() <= 64<<10 {
				t.Fatal("fixture is too small")
			}
			im := fileBinaryFile(t, data.Bytes())
			url, err := im.URL(context.Background())
			want := "data:image/" + format + ";base64," + base64.StdEncoding.EncodeToString(data.Bytes())
			if err != nil || url != want {
				t.Fatalf("original %s bytes changed: %v", format, err)
			}
			// Resolving is transient: the serialized image has no encoded payload.
			raw, err := json.Marshal(im)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil || !reflect.DeepEqual(fields, map[string]any{"path": im.Path, "sha256": im.SHA256, "bytes": float64(0)}) {
				t.Fatalf("image persisted more than its reference: %s, %v", raw, err)
			}
		})
	}
}

func TestBinaryFileURLInlineAndInvalidReferences(t *testing.T) {
	inline := BinaryFile{Path: "human-attachment.png", DataURL: "data:image/png;base64,aW5saW5l"}
	if url, err := inline.URL(context.Background()); err != nil || url != inline.DataURL {
		t.Fatal("inline human attachment changed", url, err)
	}
	checksum := strings.Repeat("a", 64)
	for name, im := range map[string]BinaryFile{
		"empty":              {},
		"path-only":          {Path: "/original.png"},
		"checksum-only":      {SHA256: checksum},
		"mixed":              {Path: "/original.png", DataURL: inline.DataURL, SHA256: checksum},
		"mixed-no-path":      {DataURL: inline.DataURL, SHA256: checksum},
		"relative-path":      {Path: "original.png", SHA256: checksum},
		"short-checksum":     {Path: "/original.png", SHA256: "abc"},
		"uppercase-checksum": {Path: "/original.png", SHA256: strings.ToUpper(checksum)},
		"nonhex-checksum":    {Path: "/original.png", SHA256: strings.Repeat("g", 64)},
	} {
		t.Run(name, func(t *testing.T) {
			if url, err := im.URL(context.Background()); err == nil || url != "" {
				t.Fatal("invalid image accepted", url, err)
			}
		})
	}
}

func TestFileBinaryFileURLSourceFailures(t *testing.T) {
	t.Run("changed", func(t *testing.T) {
		im := fileBinaryFile(t, []byte("original"))
		if err := os.WriteFile(im.Path, []byte("modified"), 0600); err != nil {
			t.Fatal(err)
		}
		var unavailable *UnavailableBinaryFileError
		if url, err := im.URL(context.Background()); !errors.As(err, &unavailable) || url != "" || !strings.Contains(err.Error(), "checksum mismatch") {
			t.Fatal("changed file accepted", url, err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		im := fileBinaryFile(t, []byte("original"))
		if err := os.Remove(im.Path); err != nil {
			t.Fatal(err)
		}
		if url, err := im.URL(context.Background()); !errors.Is(err, os.ErrNotExist) || url != "" {
			t.Fatal("missing file accepted", url, err)
		}
	})
	for _, data := range [][]byte{nil, []byte("not an image"), []byte("BM unsupported BMP data"), []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")} {
		im := fileBinaryFile(t, data)
		if url, err := im.URL(context.Background()); err == nil || url != "" || !strings.Contains(err.Error(), "unsupported image type") {
			t.Fatal("unsupported file accepted", url, err)
		}
	}
	t.Run("oversized", func(t *testing.T) {
		im := fileBinaryFile(t, nil)
		if err := os.Truncate(im.Path, maxBinaryFileBytes+1); err != nil {
			t.Fatal(err)
		}
		if url, err := im.URL(context.Background()); err == nil || url != "" || !strings.Contains(err.Error(), "exceeds") {
			t.Fatal("oversized file accepted", url, err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		im := BinaryFile{Path: t.TempDir(), SHA256: strings.Repeat("a", 64)}
		if _, err := im.URL(context.Background()); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatal("directory accepted", err)
		}
	})
	t.Run("fifo", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "fifo")
		if err := syscall.Mkfifo(path, 0600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := (BinaryFile{Path: path, SHA256: strings.Repeat("a", 64)}).URL(context.Background())
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatal("FIFO accepted", err)
			}
		case <-time.After(time.Second):
			t.Fatal("FIFO open blocked waiting for a writer")
		}
	})
}

func TestBinaryFileURLUsesCachedOriginalUntilEviction(t *testing.T) {
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	im := fileBinaryFile(t, data.Bytes())
	ctx := context.Background()
	want, err := im.URL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := blobcache.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(im.Path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := im.URL(ctx); err != nil || got != want {
		t.Fatal("overwritten source replaced cached original", err)
	}
	if err := os.Remove(im.Path); err != nil {
		t.Fatal(err)
	}
	if got, err := im.URL(ctx); err != nil || got != want {
		t.Fatal("deleted source invalidated cached original", err)
	}
	cache.TTL = time.Nanosecond
	if err := cache.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	var unavailable *UnavailableBinaryFileError
	if got, err := im.URL(ctx); !errors.As(err, &unavailable) || !errors.Is(err, os.ErrNotExist) || got != "" {
		t.Fatal("uncached missing source was not unavailable", err)
	}
	if err := os.WriteFile(im.Path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := im.URL(ctx); err != nil || got != want {
		t.Fatal("restored original did not repopulate cache", err)
	}
}

func TestBinaryFileURLMissingCacheStorageIsNotUnavailable(t *testing.T) {
	im := fileBinaryFile(t, []byte("original"))
	cache, err := blobcache.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(cache.Root); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(im.Path); err != nil {
		t.Fatal(err)
	}
	var unavailable *UnavailableBinaryFileError
	if _, err := im.URL(context.Background()); err == nil || errors.As(err, &unavailable) || !strings.Contains(err.Error(), "read cached binary file") {
		t.Fatal("missing cache storage was hidden as unavailable pixels", err)
	}
}

func TestBinaryFileURLCacheFailuresAreNotUnavailable(t *testing.T) {
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	im := fileBinaryFile(t, data.Bytes())
	cache, err := blobcache.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache.Root, "blob-"+im.SHA256+".blob"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	var unavailable *UnavailableBinaryFileError
	if _, err := im.URL(context.Background()); err == nil || errors.As(err, &unavailable) || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatal("cache corruption was hidden", err)
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", blocked)
	if _, err := im.URL(context.Background()); err == nil || errors.As(err, &unavailable) {
		t.Fatal("cache storage failure was hidden", err)
	}
}

func TestBinaryFileURLUnsupportedCacheBytesRemainFatal(t *testing.T) {
	data := []byte("not an image")
	im := fileBinaryFile(t, data)
	cache, err := blobcache.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Put(context.Background(), "original", im.SHA256, data); err != nil {
		t.Fatal(err)
	}
	var unavailable *UnavailableBinaryFileError
	if _, err := im.URL(context.Background()); err == nil || errors.As(err, &unavailable) || !strings.Contains(err.Error(), "unsupported image type") {
		t.Fatal("unsupported cached bytes were hidden as unavailable", err)
	}
}

type cancelBinaryFileReader struct {
	cancel context.CancelFunc
	reads  int
}

func (r *cancelBinaryFileReader) Read(p []byte) (int, error) {
	r.reads++
	r.cancel()
	return len(p), nil
}

func TestBinaryFileURLCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, im := range []BinaryFile{fileBinaryFile(t, []byte("original")), {DataURL: "data:image/png;base64,inline"}} {
		if url, err := im.URL(ctx); !errors.Is(err, context.Canceled) || url != "" {
			t.Fatal("canceled image accepted", url, err)
		}
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	r := &cancelBinaryFileReader{cancel: cancel}
	// Cancellation after the first chunk must prevent a second underlying read.
	if _, err := io.ReadAll(binaryContextReader{ctx, r}); !errors.Is(err, context.Canceled) || r.reads != 1 {
		t.Fatal("image reader ignored cancellation", r.reads, err)
	}
}
