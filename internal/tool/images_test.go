package tool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"ttc/internal/assets"
	"ttc/internal/blobcache"
	"ttc/internal/provider"
)

func TestReadImagesStoreOnlyOriginalPathAndChecksum(t *testing.T) {
	for _, format := range []string{"png", "jpeg", "gif"} {
		t.Run(format, func(t *testing.T) {
			r, w, x, req := toolFixture(t)
			x.BinaryFiles = (provider.ModelSpec{Images: true}).BinaryFileTypes()
			var data bytes.Buffer
			im := image.NewNRGBA(image.Rect(0, 0, 2051, 3))
			var err error
			switch format {
			case "png":
				err = png.Encode(&data, im)
			case "jpeg":
				err = jpeg.Encode(&data, im, nil)
			case "gif":
				frame := image.NewPaletted(im.Bounds(), color.Palette{color.Black, color.White})
				err = gif.Encode(&data, frame, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			// No image extension: detection must use original file contents.
			path := filepath.Join(w.Root, "source")
			if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			record := invoke(t, r, w, x, req, "read", `{"path":"source"}`)
			ok(t, record)
			var metadata struct {
				Kind, Path, SHA256, MIMEType string
				Width, Height, Bytes         int
				Truncated                    bool
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(record.Result, &fields); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(record.Result, &metadata); err != nil {
				t.Fatal(err)
			}
			var mime string
			if err := json.Unmarshal(fields["mime_type"], &mime); err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(data.Bytes())
			checksum := hex.EncodeToString(hash[:])
			if metadata.Kind != "image" || metadata.Path != path || metadata.SHA256 != checksum || metadata.Width != 2051 || metadata.Height != 3 || metadata.Bytes != data.Len() || metadata.Truncated || mime != "image/"+format {
				t.Fatalf("wrong image metadata: %s", record.Result)
			}
			if len(record.Files) != 1 || record.Files[0].Path != path || record.Files[0].SHA256 != checksum || record.Files[0].MIMEType != mime || record.Files[0].Bytes != data.Len() || record.Files[0].DataURL != "" {
				t.Fatalf("image must be a file reference, not stored pixels: %+v", record.Files)
			}
			cache, err := blobcache.Default()
			if err != nil {
				t.Fatal(err)
			}
			cached, err := cache.Get(context.Background(), "original", checksum)
			if err != nil || !bytes.Equal(cached, data.Bytes()) {
				t.Fatal("read did not cache exact validated bytes", err)
			}
			version, encoded, err := record.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(encoded, []byte("data_url")) || bytes.Contains(encoded, []byte("base64")) || fields["snapshot"] != nil || len(encoded) > 4096 {
				t.Fatalf("record stored payload or a copied snapshot: %s", encoded)
			}
			codec, _ := r.Get("read")
			decoded, err := codec.DecodeRecord(version, encoded)
			if err != nil || !reflect.DeepEqual(record, decoded) {
				t.Fatalf("image record did not round trip: %v", err)
			}
			original, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(original, data.Bytes()) {
				t.Fatal("read changed source bytes", err)
			}
		})
	}
}

func TestReadImageCacheFailureHasNoAttachment(t *testing.T) {
	r, w, x, req := toolFixture(t)
	x.BinaryFiles = (provider.ModelSpec{Images: true}).BinaryFileTypes()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Root, "image.png"), data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", blocked)
	record := invoke(t, r, w, x, req, "read", `{"path":"image.png"}`)
	if len(record.Files) != 0 || !strings.Contains(string(record.Result), `"ok":false`) || !strings.Contains(string(record.Result), "open binary cache") {
		t.Fatal("read silently ignored cache failure", string(record.Result))
	}
}

func TestReadImageErrorsAndTextDetection(t *testing.T) {
	r, w, x, req := toolFixture(t)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 4, 2))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(w.Root, "image.png")
	if err := os.WriteFile(path, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args, code string
		vision     bool
	}{
		{`{"path":"image.png"}`, "unsupported_binary_input", false},
		{`{"path":"image.png","offset":1}`, "invalid_input", true},
		{`{"path":"image.png","limit":200}`, "invalid_input", true},
	} {
		x.BinaryFiles = (provider.ModelSpec{Images: test.vision}).BinaryFileTypes()
		record := invoke(t, r, w, x, req, "read", test.args)
		if len(record.Files) != 0 || !strings.Contains(string(record.Result), `"code":"`+test.code+`"`) {
			t.Fatalf("missing %s or error carried image: %s", test.code, record.Result)
		}
	}
	x.BinaryFiles = (provider.ModelSpec{Images: true}).BinaryFileTypes()
	for _, test := range []struct {
		name string
		data []byte
	}{
		{"bad.png", []byte("\x89PNG\r\n\x1a\nmalformed")},
		{"binary", []byte{0, 1, 2}},
		{"bad.gif", []byte("GIF89abad image")},
	} {
		if err := os.WriteFile(filepath.Join(w.Root, test.name), test.data, 0600); err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]string{"path": test.name})
		record := invoke(t, r, w, x, req, "read", string(args))
		if len(record.Files) != 0 || !strings.Contains(string(record.Result), `"code":"unsupported_content"`) {
			t.Fatalf("invalid bytes accepted: %s", record.Result)
		}
	}
	if err := os.WriteFile(filepath.Join(w.Root, "text.png"), []byte("ordinary text\n"), 0600); err != nil {
		t.Fatal(err)
	}
	record := invoke(t, r, w, x, req, "read", `{"path":"text.png","limit":1}`)
	ok(t, record)
	if len(record.Files) != 0 || !strings.Contains(string(record.Result), `"kind":"file"`) {
		t.Fatalf("extension overrode text content: %s", record.Result)
	}

	oversized := filepath.Join(w.Root, "oversized")
	if err := os.WriteFile(oversized, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(oversized, assets.MaxBytes+1); err != nil {
		t.Fatal(err)
	}
	record = invoke(t, r, w, x, req, "read", `{"path":"oversized"}`)
	if !strings.Contains(string(record.Result), "32 MiB") || len(record.Files) != 0 {
		t.Fatalf("oversized image accepted: %s", record.Result)
	}
	// Change the PNG IHDR dimensions and CRC without allocating its pixels.
	pixels := append([]byte(nil), encoded.Bytes()...)
	binary.BigEndian.PutUint32(pixels[16:20], assets.MaxPixels)
	binary.BigEndian.PutUint32(pixels[20:24], 2)
	binary.BigEndian.PutUint32(pixels[29:33], crc32.ChecksumIEEE(pixels[12:29]))
	if err := os.WriteFile(filepath.Join(w.Root, "pixels"), pixels, 0600); err != nil {
		t.Fatal(err)
	}
	record = invoke(t, r, w, x, req, "read", `{"path":"pixels"}`)
	if !strings.Contains(string(record.Result), "pixels") || !strings.Contains(string(record.Result), `"ok":false`) || len(record.Files) != 0 {
		t.Fatalf("pixel limit not enforced: %s", record.Result)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	record = r.Invoke(ctx, x, "read", json.RawMessage(`{"path":"image.png"}`))
	if !strings.Contains(string(record.Result), `"code":"cancelled"`) || len(record.Files) != 0 {
		t.Fatalf("canceled image read accepted: %s", record.Result)
	}
}

func TestImageReadUsesOpenedDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image")
	var original bytes.Buffer
	if err := png.Encode(&original, image.NewNRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	value, err := readOpenedPage(context.Background(), f, path, 1, 200, (provider.ModelSpec{Images: true}).BinaryFileTypes())
	if err != nil {
		t.Fatal(err)
	}
	im, ok := value.(binaryRead)
	if !ok || !bytes.Equal(im.data, original.Bytes()) {
		t.Fatal("image read reopened replaced source", value)
	}
}

func TestReadNonAnimatedGIFCanvasAndContainer(t *testing.T) {
	r, w, x, req := toolFixture(t)
	x.BinaryFiles = (provider.ModelSpec{Images: true}).BinaryFileTypes()
	palette := color.Palette{color.Black, color.White}
	frame := image.NewPaletted(image.Rect(2, 3, 4, 5), palette)
	var encoded bytes.Buffer
	if err := gif.EncodeAll(&encoded, &gif.GIF{
		Image: []*image.Paletted{frame}, Delay: []int{0},
		Config: image.Config{ColorModel: palette, Width: 10, Height: 12},
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(w.Root, "offset.gif")
	if err := os.WriteFile(path, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	record := invoke(t, r, w, x, req, "read", `{"path":"offset.gif"}`)
	ok(t, record)
	var metadata struct{ Width, Height int }
	if err := json.Unmarshal(record.Result, &metadata); err != nil || metadata.Width != 10 || metadata.Height != 12 {
		t.Fatalf("GIF metadata must describe the canvas, not its smaller frame: %s, %v", record.Result, err)
	}
	hash := sha256.Sum256(encoded.Bytes())
	if record.Files[0].SHA256 != hex.EncodeToString(hash[:]) {
		t.Fatal("GIF was flattened or recompressed")
	}
	// Every proper prefix lacks the required trailer or an earlier container
	// boundary, including the complete first-frame data without its trailer.
	for length := 0; length < encoded.Len(); length++ {
		if _, err := validateReadImage(encoded.Bytes()[:length]); err == nil {
			t.Fatalf("accepted incomplete GIF prefix of %d bytes", length)
		}
	}
	var animation bytes.Buffer
	if err := gif.EncodeAll(&animation, &gif.GIF{
		Image: []*image.Paletted{frame, frame}, Delay: []int{1, 2},
		Config: image.Config{ColorModel: palette, Width: 10, Height: 12},
	}); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"animated.gif":   animation.Bytes(),
		"incomplete.gif": encoded.Bytes()[:encoded.Len()-1],
	} {
		if err := os.WriteFile(filepath.Join(w.Root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]string{"path": name})
		record := invoke(t, r, w, x, req, "read", string(args))
		if !strings.Contains(string(record.Result), `"code":"unsupported_content"`) || len(record.Files) != 0 {
			t.Fatalf("animated/incomplete GIF accepted: %s", record.Result)
		}
		if name == "animated.gif" && !strings.Contains(string(record.Result), "non-animated GIF") {
			t.Fatalf("animation rejection must be actionable: %s", record.Result)
		}
	}
}
