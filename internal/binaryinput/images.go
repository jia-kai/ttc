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
	"ttc/internal/prompts"
)

// MaxImagePixels bounds the canvas pixel count before decoded image allocation.
const MaxImagePixels = 16 << 20

// ImageConfig validates the encoded byte size and canvas dimensions without
// allocating pixels. It returns decoder format (png, jpeg or gif) and header
// metadata; complete image/container validation requires decoding separately.
func ImageConfig(data []byte) (image.Config, string, error) {
	if len(data) > blobcache.MaxBytes {
		return image.Config{}, "", fmt.Errorf(prompts.ImageTooLarge, blobcache.MaxBytes)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return image.Config{}, "", fmt.Errorf(prompts.ImageHeaderDecode, err)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > MaxImagePixels/config.Height {
		return image.Config{}, "", fmt.Errorf(prompts.ImageInvalidDimensions, MaxImagePixels)
	}
	return config, format, nil
}

// DecodeImage decodes PNG, JPEG or a complete single-frame GIF. It bounds
// encoded size and canvas dimensions before allocating pixels and observes
// cancellation during reads and GIF container validation.
func DecodeImage(ctx context.Context, data []byte) (image.Image, error) {
	_, pixels, err := decodeImage(ctx, data)
	return pixels, err
}

func validateReadImage(ctx context.Context, data []byte) (image.Config, error) {
	config, _, err := decodeImage(ctx, data)
	return config, err
}

// GIFs need full container validation: image.Decode otherwise stops after the
// first frame and can accept an animation or a missing trailer. Count frames
// before DecodeAll so hostile animations cannot allocate unbounded frames.
func decodeImage(ctx context.Context, data []byte) (image.Config, image.Image, error) {
	if err := ctx.Err(); err != nil {
		return image.Config{}, nil, err
	}
	config, format, err := ImageConfig(data)
	if err != nil {
		return image.Config{}, nil, err
	}
	var pixels image.Image
	switch format {
	case "gif":
		if err := singleFrameGIF(ctx, data); err != nil {
			return image.Config{}, nil, err
		}
		var frames *gif.GIF
		frames, err = gif.DecodeAll(&contextReader{ctx: ctx, reader: bytes.NewReader(data)})
		if err == nil {
			pixels = frames.Image[0]
		}
	case "png", "jpeg":
		pixels, _, err = image.Decode(&contextReader{ctx: ctx, reader: bytes.NewReader(data)})
	default:
		return image.Config{}, nil, fmt.Errorf(prompts.BinaryImageFormatUnsupported, format)
	}
	if ctx.Err() != nil {
		return image.Config{}, nil, ctx.Err()
	}
	if err != nil {
		return image.Config{}, nil, fmt.Errorf(prompts.BinaryInvalidImage, err)
	}
	return config, pixels, nil
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
				return fmt.Errorf(prompts.ImageGIFSingleFrame)
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
				return fmt.Errorf(prompts.ImageGIFAnimated)
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
			return fmt.Errorf(prompts.BinaryGIFBlockMarker, marker)
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
