package tool

import (
	"bytes"
	"fmt"
	"image"
	"image/gif"
	"io"

	"ttc/internal/assets"
)

// validateReadImage bounds the canvas before pixel allocation. GIFs need full
// container validation: image.Decode otherwise stops after the first frame and
// can accept an animation or a missing trailer. Count frames before DecodeAll so
// hostile animations cannot allocate an unbounded collection of decoded frames.
func validateReadImage(data []byte) (image.Config, error) {
	config, format, err := assets.Config(data)
	if err != nil {
		return image.Config{}, err
	}
	if format == "gif" {
		if err := singleFrameGIF(data); err != nil {
			return image.Config{}, err
		}
		_, err = gif.DecodeAll(bytes.NewReader(data))
	} else {
		_, _, err = image.Decode(bytes.NewReader(data))
	}
	if err != nil {
		return image.Config{}, fmt.Errorf("invalid image: %w", err)
	}
	return config, nil
}

func singleFrameGIF(data []byte) error {
	// Header plus logical screen descriptor. Config has already validated the
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
