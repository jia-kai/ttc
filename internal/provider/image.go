package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"ttc/internal/blobcache"
)

const maxImageBytes = blobcache.MaxBytes

// UnavailableImageError reports a valid reference whose original bytes are no
// longer cached and cannot be reconstructed from its source. Adapters represent
// this as an explicit text notice, preserving canonical history and tool IDs.
// Invalid references, cancellation and cache failures are not unavailable images.
type UnavailableImageError struct {
	Image Image
	Err   error // Failure to reconstruct the original bytes from Image.Path.
}

// Error describes the omitted attachment and its failed source verification.
func (e *UnavailableImageError) Error() string {
	return fmt.Sprintf("image unavailable: original pixels omitted for %q (expected SHA-256 %s); cache miss and source could not be verified: %v", e.Image.Path, e.Image.SHA256, e.Err)
}

// Unwrap returns the source reconstruction error.
func (e *UnavailableImageError) Unwrap() error { return e.Err }

// URL returns an inline attachment unchanged, or resolves original encoded
// bytes cache-first into a transport-only data URL. On a miss it verifies the
// source and repopulates the shared cache. Reads are bounded to 32 MiB and
// cancelable; nonregular descriptors are rejected without blocking on FIFOs.
// Missing/changed sources return UnavailableImageError, never substitute new
// contents. Decoded pixel limits are checked by read(), not request assembly.
func (im Image) URL(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if im.DataURL != "" {
		if im.SHA256 != "" {
			return "", errors.New("image mixes inline data and file checksum")
		}
		return im.DataURL, nil
	}
	if im.Path == "" || im.SHA256 == "" {
		return "", errors.New("file image requires an absolute path and SHA-256 checksum")
	}
	if !filepath.IsAbs(im.Path) {
		return "", fmt.Errorf("file image path must be absolute: %q", im.Path)
	}
	if len(im.SHA256) != sha256.Size*2 || strings.Trim(im.SHA256, "0123456789abcdef") != "" {
		return "", fmt.Errorf("file image %q requires a lowercase hex SHA-256 checksum", im.Path)
	}
	url, err := im.fileURL(ctx)
	if err != nil {
		return "", fmt.Errorf("file image %q: %w", im.Path, err)
	}
	return url, nil
}

func (im Image) fileURL(ctx context.Context) (string, error) {
	cache, err := blobcache.Default()
	if err != nil {
		return "", fmt.Errorf("open image cache: %w", err)
	}
	data, err := cache.Get(ctx, "original", im.SHA256)
	cached := err == nil
	if errors.Is(err, blobcache.ErrMiss) {
		data, err = im.sourceBytes(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", &UnavailableImageError{Image: im, Err: err}
		}
	} else if err != nil {
		return "", fmt.Errorf("read cached image: %w", err)
	}
	// Cached bytes are an external boundary. sourceBytes already verified a
	// miss result, so only hits need another checksum check.
	if cached {
		checksum := sha256.Sum256(data)
		if hex.EncodeToString(checksum[:]) != im.SHA256 {
			return "", errors.New("cached image SHA-256 checksum mismatch")
		}
	}
	mime := http.DetectContentType(data)
	switch mime {
	case "image/png", "image/jpeg", "image/gif":
	default:
		err := fmt.Errorf("unsupported image type %q; expected PNG, JPEG or GIF", mime)
		if !cached {
			return "", &UnavailableImageError{Image: im, Err: err}
		}
		return "", fmt.Errorf("cached image: %w", err)
	}
	if !cached {
		if err := cache.Put(ctx, "original", im.SHA256, data); err != nil {
			return "", fmt.Errorf("cache reconstructed image: %w", err)
		}
	}
	url := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return url, nil
}

func (im Image) sourceBytes(ctx context.Context) ([]byte, error) {
	// Validate the opened descriptor rather than a racy pathname stat. Linux
	// ignores O_NONBLOCK for regular files, but it prevents waiting on a FIFO.
	f, err := os.OpenFile(im.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer stop()
	st, err := f.Stat()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("source is not a regular file")
	}
	if st.Size() > maxImageBytes {
		return nil, fmt.Errorf("source exceeds %d bytes", maxImageBytes)
	}
	data, err := io.ReadAll(io.LimitReader(imageContextReader{ctx, f}, maxImageBytes+1))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if len(data) > maxImageBytes {
		return nil, fmt.Errorf("source exceeds %d bytes", maxImageBytes)
	}
	checksum := sha256.Sum256(data)
	if hex.EncodeToString(checksum[:]) != im.SHA256 {
		return nil, errors.New("source SHA-256 checksum mismatch; original image has changed")
	}
	return data, nil
}

type imageContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r imageContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	// io.ReadAll grows its buffer; keep cancellation checks at most 64 KiB apart.
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	return r.r.Read(p)
}
