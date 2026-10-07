package binaryinput

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"strings"
	"testing"

	"ttc/internal/blobcache"
)

func TestImageConfigBoundsBeforePixelAllocation(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	config, format, err := ImageConfig(encoded.Bytes())
	if err != nil || format != "png" || config.Width != 3 || config.Height != 2 {
		t.Fatalf("wrong header metadata: %+v %q %v", config, format, err)
	}
	if _, _, err := ImageConfig(make([]byte, blobcache.MaxBytes+1)); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("encoded byte bound ignored: %v", err)
	}
	for _, dims := range [][2]uint32{{MaxImagePixels, 2}, {0, 1}, {1, 0}} {
		data := bytes.Clone(encoded.Bytes())
		binary.BigEndian.PutUint32(data[16:20], dims[0])
		binary.BigEndian.PutUint32(data[20:24], dims[1])
		binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
		if _, _, err := ImageConfig(data); err == nil {
			t.Fatalf("invalid canvas %v accepted", dims)
		}
	}
}

// cancelOnCheckContext deterministically cancels after work has started, without
// timing assumptions or a background goroutine racing a small fixture's decode.
type cancelOnCheckContext struct {
	context.Context
	cancel   context.CancelFunc
	checks   int
	cancelAt int
}

func (c *cancelOnCheckContext) Err() error {
	c.checks++
	if c.cancelAt > 0 && c.checks == c.cancelAt {
		c.cancel()
	}
	return c.Context.Err()
}

func TestImageValidationCancellationDuringDecode(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 256, 256))
	state := uint32(1)
	for i := range img.Pix {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		img.Pix[i] = byte(state)
	}
	for name, encode := range map[string]func(io.Writer) error{
		"png":  func(w io.Writer) error { return png.Encode(w, img) },
		"jpeg": func(w io.Writer) error { return jpeg.Encode(w, img, nil) },
		"gif":  func(w io.Writer) error { return gif.Encode(w, img, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			var encoded bytes.Buffer
			if err := encode(&encoded); err != nil {
				t.Fatal(err)
			}
			baseline := &cancelOnCheckContext{Context: context.Background()}
			if _, err := validateReadImage(baseline, encoded.Bytes()); err != nil {
				t.Fatal(err)
			}
			if baseline.checks < 6 {
				t.Fatalf("fixture did not exercise incremental decode: %d checks", baseline.checks)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// The last check is after decode. Cancel before its last reader
			// checkpoint, after container scanning and decoding have started.
			counted := &cancelOnCheckContext{Context: ctx, cancel: cancel, cancelAt: baseline.checks - 2}
			if _, err := validateReadImage(counted, encoded.Bytes()); !errors.Is(err, context.Canceled) {
				t.Fatalf("decode cancellation ignored after %d checks: %v", counted.checks, err)
			}
		})
	}
}

func TestGIFTraversalCancellationAfterStart(t *testing.T) {
	var encoded bytes.Buffer
	if err := gif.Encode(&encoded, image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White}), nil); err != nil {
		t.Fatal(err)
	}
	data := append(bytes.Clone(encoded.Bytes()[:encoded.Len()-1]), 0x21, 0xfe)
	for range 1000 {
		data = append(data, 1, 'x')
	}
	data = append(data, 0, 0x3b)
	if err := singleFrameGIF(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	counted := &cancelOnCheckContext{Context: ctx, cancel: cancel, cancelAt: 20}
	if err := singleFrameGIF(counted, data); !errors.Is(err, context.Canceled) {
		t.Fatalf("GIF traversal cancellation ignored: %v", err)
	}
}
