package binaryinput

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"

	"ttc/internal/blobcache"
)

// MaxImagePixels bounds the canvas pixel count before decoded image allocation.
const MaxImagePixels = 16 << 20

// ImageConfig validates the encoded byte size and canvas dimensions without
// allocating pixels. It returns decoder format (png, jpeg or gif) and header
// metadata; complete image/container validation requires decoding separately.
func ImageConfig(data []byte) (image.Config, string, error) {
	if len(data) > blobcache.MaxBytes {
		return image.Config{}, "", fmt.Errorf("image exceeds %d bytes; resize or compress it before retrying", blobcache.MaxBytes)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return image.Config{}, "", fmt.Errorf("cannot decode image header; provide a valid PNG, JPEG or GIF: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > MaxImagePixels/config.Height {
		return image.Config{}, "", fmt.Errorf("image dimensions are invalid or exceed %d pixels; provide a valid image with fewer pixels", MaxImagePixels)
	}
	return config, format, nil
}

// validateReadImage bounds the canvas before pixel allocation. GIFs need full
// container validation: image.Decode otherwise stops after the first frame and
// can accept an animation or a missing trailer. Count frames before DecodeAll so
// hostile animations cannot allocate an unbounded collection of decoded frames.
func validateReadImage(ctx context.Context, data []byte) (image.Config, error) {
	if err := ctx.Err(); err != nil {
		return image.Config{}, err
	}
	config, format, err := ImageConfig(data)
	if err != nil {
		return image.Config{}, err
	}
	if format == "gif" {
		if err := singleFrameGIF(ctx, data); err != nil {
			return image.Config{}, err
		}
		_, err = gif.DecodeAll(&contextReader{ctx: ctx, reader: bytes.NewReader(data)})
	} else {
		_, _, err = image.Decode(&contextReader{ctx: ctx, reader: bytes.NewReader(data)})
	}
	if ctx.Err() != nil {
		return image.Config{}, ctx.Err()
	}
	if err != nil {
		return image.Config{}, fmt.Errorf("invalid image: %w", err)
	}
	return config, nil
}

func singleFrameGIF(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Header plus logical screen descriptor. ImageConfig has already validated the
	// header; this pass walks container boundaries without allocating pixels.
	if len(data) < 13 {
		return io.ErrUnexpectedEOF
	}
	offset := 13
	skip := func(n int) error {
		if n > len(data)-offset {
			return io.ErrUnexpectedEOF
		}
		offset += n
		return nil
	}
	blocks := func() error {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			if offset == len(data) {
				return io.ErrUnexpectedEOF
			}
			n := int(data[offset])
			offset++
			if n == 0 {
				return nil
			}
			if err := skip(n); err != nil {
				return err
			}
		}
	}
	if packed := data[10]; packed&0x80 != 0 {
		if err := skip(3 << ((packed & 7) + 1)); err != nil {
			return err
		}
	}
	frames := 0
	for offset < len(data) {
		if err := ctx.Err(); err != nil {
			return err
		}
		marker := data[offset]
		offset++
		switch marker {
		case 0x3b: // Trailer.
			if frames != 1 {
				return fmt.Errorf("GIF must contain exactly one image frame")
			}
			return nil
		case 0x21: // Extension label, followed by data subblocks.
			if err := skip(1); err != nil {
				return err
			}
			if err := blocks(); err != nil {
				return err
			}
		case 0x2c: // Image descriptor, optional color table, then LZW data.
			frames++
			if frames > 1 {
				return fmt.Errorf("animated GIF input is unsupported; provide a non-animated GIF, PNG or JPEG")
			}
			if len(data)-offset < 9 {
				return io.ErrUnexpectedEOF
			}
			packed := data[offset+8]
			offset += 9
			if packed&0x80 != 0 {
				if err := skip(3 << ((packed & 7) + 1)); err != nil {
					return err
				}
			}
			if err := skip(1); err != nil { // LZW minimum code size.
				return err
			}
			if err := blocks(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("invalid GIF block marker %#x", marker)
		}
	}
	return io.ErrUnexpectedEOF
}

// contextReader keeps decoding synchronous and checks cancellation between
// bounded reads. A decoder can finish processing its current buffered input,
// but cannot keep consuming the original after cancellation.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

const binaryReadChunk = 32 << 10

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p[:min(len(p), binaryReadChunk)])
	if canceled := r.ctx.Err(); canceled != nil {
		return n, canceled
	}
	return n, err
}
