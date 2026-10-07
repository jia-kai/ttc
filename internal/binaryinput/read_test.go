package binaryinput

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/blobcache"
	"ttc/internal/llm"
)

func zipFixture(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for name, content := range entries {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestDocumentZIPMetadataIsBounded(t *testing.T) {
	entries := make(map[string]string, 4097)
	for i := 0; i < 4097; i++ {
		entries[fmt.Sprintf("entry-%d", i)] = "x"
	}
	if _, err := boundedDocumentZIP(zipFixture(t, entries)); err == nil || !strings.Contains(err.Error(), "4096") {
		t.Fatalf("unbounded ZIP directory accepted: %v", err)
	}
	data := zipFixture(t, map[string]string{"[Content_Types].xml": "<Types/>", "word/document.xml": "<document/>"})
	for name, modify := range map[string]func([]byte){
		"forged-count": func(data []byte) {
			end := data[len(data)-22:]
			binary.LittleEndian.PutUint16(end[8:10], 1)
			binary.LittleEndian.PutUint16(end[10:12], 1)
		},
		"zip64":         func(data []byte) { binary.LittleEndian.PutUint16(data[len(data)-22+10:], 0xffff) },
		"central-bound": func(data []byte) { binary.LittleEndian.PutUint32(data[len(data)-22+12:], 2<<20+1) },
	} {
		bad := bytes.Clone(data)
		modify(bad)
		if _, err := boundedDocumentZIP(bad); err == nil {
			t.Fatalf("invalid %s ZIP accepted", name)
		}
	}
	for length := 0; length < len(data); length++ {
		if _, err := boundedDocumentZIP(data[:length]); err == nil {
			t.Fatalf("truncated ZIP prefix of %d bytes accepted", length)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := validateReadDocument(ctx, data, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled container validation returned %v", err)
	}
	for mime, index := range map[string]string{"application/vnd.apple.pages": "index.xml", "application/vnd.apple.keynote": "index.apxl.gz"} {
		if err := validateReadDocument(context.Background(), zipFixture(t, map[string]string{index: "original bytes"}), mime); err != nil {
			t.Fatalf("legacy iWork ZIP index rejected: %v", err)
		}
	}
}

func TestImageValidatorRejectsEveryGIFPrefix(t *testing.T) {
	palette := color.Palette{color.Black, color.White}
	frame := image.NewPaletted(image.Rect(2, 3, 4, 5), palette)
	var encoded bytes.Buffer
	if err := gif.EncodeAll(&encoded, &gif.GIF{Image: []*image.Paletted{frame}, Delay: []int{0}, Config: image.Config{ColorModel: palette, Width: 10, Height: 12}}); err != nil {
		t.Fatal(err)
	}
	for length := 0; length < encoded.Len(); length++ {
		if _, err := validateReadImage(context.Background(), encoded.Bytes()[:length]); err == nil {
			t.Fatalf("accepted incomplete GIF prefix of %d bytes", length)
		}
	}
}

func TestReadTextDoesNotConsumeBytes(t *testing.T) {
	for _, text := range []string{"", "short", "first\nsecond\n"} {
		reader := bufio.NewReader(strings.NewReader(text))
		result, err := Read(context.Background(), reader, "source.txt", int64(len(text)), nil)
		if err != nil || result != nil {
			t.Fatalf("text classified as binary: %+v %v", result, err)
		}
		remaining, err := io.ReadAll(reader)
		if err != nil || string(remaining) != text {
			t.Fatalf("text consumed: %q %v", remaining, err)
		}
	}
}

func TestReadLimitsAndCancellation(t *testing.T) {
	data := []byte("%PDF-1.7\n%%EOF\n")
	capability := llm.BinaryFileType{Kind: "document", MIMEType: "application/pdf", Extensions: []string{".pdf"}, MaxBytes: len(data) - 1}
	for _, size := range []int64{0, int64(len(data))} {
		_, err := Read(context.Background(), bufio.NewReader(bytes.NewReader(data)), "source.pdf", size, []llm.BinaryFileType{capability})
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "binary_too_large" {
			t.Fatalf("byte bound ignored: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Read(ctx, bufio.NewReader(bytes.NewReader(data)), "source.pdf", 0, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestBoundedBinaryRead(t *testing.T) {
	data := bytes.Repeat([]byte("original"), binaryReadChunk)
	for _, size := range []int64{-1, 0, 1, int64(len(data)), int64(len(data) + 10)} {
		for _, limit := range []int{len(data) - 1, len(data), len(data) + 1} {
			got, overflow, err := readBinaryBytes(context.Background(), bytes.NewReader(data), size, limit)
			if err != nil || overflow != (len(data) > limit) {
				t.Fatalf("size %d limit %d: overflow=%v err=%v", size, limit, overflow, err)
			}
			if !overflow && !bytes.Equal(got, data) {
				t.Fatalf("size %d limit %d: original changed", size, limit)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	counted := &cancelOnCheckContext{Context: ctx, cancel: cancel, cancelAt: 5}
	reader := bytes.NewReader(data)
	if _, _, err := readBinaryBytes(counted, reader, int64(len(data)), len(data)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation after read start ignored: %v", err)
	}
	if reader.Len() == 0 || reader.Len() == len(data) {
		t.Fatalf("cancellation did not interrupt an in-progress read: %d bytes remain", reader.Len())
	}
}

func BenchmarkBoundedBinaryReadNearLimit(b *testing.B) {
	data := make([]byte, blobcache.MaxBytes)
	for _, name := range []string{"stat-preallocated", "readall"} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				reader := bytes.NewReader(data)
				if name == "readall" {
					if _, err := io.ReadAll(io.LimitReader(reader, int64(blobcache.MaxBytes)+1)); err != nil {
						b.Fatal(err)
					}
				} else if _, overflow, err := readBinaryBytes(context.Background(), reader, int64(len(data)), blobcache.MaxBytes); err != nil || overflow {
					b.Fatalf("overflow=%v err=%v", overflow, err)
				}
			}
		})
	}
}

func TestReadAndStoreContentDetectedImage(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	result, err := Read(context.Background(), bufio.NewReader(bytes.NewReader(data)), "misleading.pdf", int64(len(data)), (llm.ModelSpec{Images: true}).BinaryFileTypes())
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Kind != "image" || result.MIMEType != "image/png" || result.Width != 3 || result.Height != 2 || !bytes.Equal(result.Data, data) {
		t.Fatalf("wrong classification: %+v", result)
	}
	ref, err := result.Store(context.Background(), "missing-original")
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	if ref.SHA256 != hex.EncodeToString(hash[:]) || ref.Path != "missing-original" || ref.Bytes != len(data) || ref.MIMEType != "image/png" {
		t.Fatalf("wrong reference: %+v", ref)
	}
	cache, err := blobcache.Default()
	if err != nil {
		t.Fatal(err)
	}
	cached, err := cache.Get(context.Background(), "original", ref.SHA256)
	if err != nil || !bytes.Equal(cached, data) {
		t.Fatalf("wrong original cache bytes: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := result.Store(ctx, "missing-original"); !errors.Is(err, context.Canceled) {
		t.Fatalf("store cancellation ignored: %v", err)
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", blocked)
	if _, err := result.Store(context.Background(), "missing-original"); err == nil || !strings.Contains(err.Error(), "open binary cache") {
		t.Fatalf("cache failure ignored: %v", err)
	}
}
