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
)

const maxImageBytes = 32 << 20

// URL returns an inline attachment unchanged, or loads a file reference's
// original encoded bytes into a transport-only data URL. File references require
// an absolute path and lowercase SHA-256 checksum. Reads are bounded to 32 MiB,
// check cancellation between chunks, and reject nonregular descriptors without
// blocking on FIFOs. Missing, changed and unsupported files are errors; no files
// are copied, resized or recompressed. Decoded pixel limits are checked by read()
// before a reference is persisted, not during request assembly.
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
	// Validate the opened descriptor rather than a racy pathname stat. Linux
	// ignores O_NONBLOCK for regular files, but it prevents waiting on a FIFO.
	f, err := os.OpenFile(im.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer stop()
	st, err := f.Stat()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", errors.New("source is not a regular file")
	}
	if st.Size() > maxImageBytes {
		return "", fmt.Errorf("source exceeds %d bytes", maxImageBytes)
	}
	data, err := io.ReadAll(io.LimitReader(imageContextReader{ctx, f}, maxImageBytes+1))
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	if len(data) > maxImageBytes {
		return "", fmt.Errorf("source exceeds %d bytes", maxImageBytes)
	}
	checksum := sha256.Sum256(data)
	if hex.EncodeToString(checksum[:]) != im.SHA256 {
		return "", errors.New("source SHA-256 checksum mismatch; original image has changed")
	}
	mime := http.DetectContentType(data)
	switch mime {
	case "image/png", "image/jpeg", "image/gif":
	default:
		return "", fmt.Errorf("unsupported image type %q; expected PNG, JPEG or GIF", mime)
	}
	url := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return url, nil
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
