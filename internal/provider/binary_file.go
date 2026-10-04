package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"ttc/internal/blobcache"
)

const maxBinaryFileBytes = blobcache.MaxBytes

// UnavailableBinaryFileError reports a valid reference whose original bytes are
// no longer cached and cannot be reconstructed. Adapters emit an explicit notice,
// preserving history and tool IDs. Invalid references and cache failures are fatal.
type UnavailableBinaryFileError struct {
	File BinaryFile
	Err  error // Failure to reconstruct the original bytes from File.Path.
}

// Error describes the omitted original and failed source verification.
func (e *UnavailableBinaryFileError) Error() string {
	return fmt.Sprintf("binary file unavailable: original bytes omitted for %q (expected SHA-256 %s); cache miss and source could not be verified: %v", e.File.Path, e.File.SHA256, e.Err)
}

// Unwrap returns the source reconstruction error.
func (e *UnavailableBinaryFileError) Unwrap() error { return e.Err }

// URL validates an inline data URL or resolves checksum-verified originals
// cache-first into a transport-only data URL. Reads are cancelable and bounded
// to 32 MiB; nonregular descriptors are rejected without blocking on FIFOs.
// Missing/changed sources return UnavailableBinaryFileError, never new contents.
// File-backed documents require explicit MIME and byte size; image MIME can be
// detected from original bytes. Pixel limits are checked by read().
func (f BinaryFile) URL(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if f.Bytes < 0 || f.Bytes > maxBinaryFileBytes {
		return "", fmt.Errorf("binary file size must be between 0 and %d bytes", maxBinaryFileBytes)
	}
	if f.MIMEType != "" && !validBinaryMIME(f.MIMEType) {
		return "", errors.New("binary file requires a canonical MIME type without parameters")
	}
	if f.DataURL != "" {
		if f.SHA256 != "" {
			return "", errors.New("binary file mixes inline data and file checksum")
		}
		mt, size, err := inlineBinaryInfo(f.DataURL)
		if err != nil {
			return "", err
		}
		if f.MIMEType != "" && f.MIMEType != mt {
			return "", errors.New("binary file MIME type differs from data URL")
		}
		if f.Bytes != 0 && f.Bytes != size {
			return "", errors.New("binary file byte size differs from data URL")
		}
		return f.DataURL, ctx.Err()
	}
	if f.Path == "" || f.SHA256 == "" {
		return "", errors.New("file-backed binary requires an absolute path and SHA-256 checksum")
	}
	if !filepath.IsAbs(f.Path) {
		return "", fmt.Errorf("binary file path must be absolute: %q", f.Path)
	}
	if len(f.SHA256) != sha256.Size*2 || strings.Trim(f.SHA256, "0123456789abcdef") != "" {
		return "", fmt.Errorf("binary file %q requires a lowercase hex SHA-256 checksum", f.Path)
	}
	if f.MIMEType != "" && !strings.HasPrefix(f.MIMEType, "image/") && f.Bytes == 0 {
		return "", errors.New("file-backed document requires original byte size")
	}
	url, err := f.fileURL(ctx)
	if err != nil {
		return "", fmt.Errorf("binary file %q: %w", f.Path, err)
	}
	return url, nil
}

func validBinaryMIME(mt string) bool {
	parsed, params, err := mime.ParseMediaType(mt)
	return err == nil && len(params) == 0 && parsed == mt && strings.Contains(mt, "/")
}

// inlineBinaryInfo validates the base64 data URL without allocating decoded bytes.
func inlineBinaryInfo(url string) (string, int, error) {
	header, data, ok := strings.Cut(url, ",")
	if !ok || !strings.HasPrefix(header, "data:") || !strings.HasSuffix(header, ";base64") {
		return "", 0, errors.New("binary file requires a base64 data URL with explicit MIME type")
	}
	mt := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
	if !validBinaryMIME(mt) {
		return "", 0, errors.New("data URL requires a canonical MIME type without parameters")
	}
	if len(data) > base64.StdEncoding.EncodedLen(maxBinaryFileBytes) {
		return "", 0, fmt.Errorf("inline binary file exceeds %d bytes", maxBinaryFileBytes)
	}
	if strings.ContainsAny(data, "\r\n") {
		return "", 0, errors.New("binary file base64 must not contain whitespace")
	}
	n, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(data)))
	if err != nil {
		return "", 0, fmt.Errorf("invalid binary file base64: %w", err)
	}
	if n == 0 || n > maxBinaryFileBytes {
		return "", 0, fmt.Errorf("inline binary file size must be between 1 and %d bytes", maxBinaryFileBytes)
	}
	return mt, int(n), nil
}

func (f BinaryFile) fileURL(ctx context.Context) (string, error) {
	cache, err := blobcache.Default()
	if err != nil {
		return "", fmt.Errorf("open binary file cache: %w", err)
	}
	data, err := cache.Get(ctx, "original", f.SHA256)
	cached := err == nil
	if errors.Is(err, blobcache.ErrMiss) {
		data, err = f.sourceBytes(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", &UnavailableBinaryFileError{File: f, Err: err}
		}
	} else if err != nil {
		return "", fmt.Errorf("read cached binary file: %w", err)
	}
	// Get verifies content-addressed cache bytes; sourceBytes verifies misses.
	if f.Bytes != 0 && f.Bytes != len(data) {
		return "", errors.New("binary file byte size differs from original")
	}
	mt := f.MIMEType
	if mt == "" || strings.HasPrefix(mt, "image/") {
		detected := http.DetectContentType(data)
		if !isImageMIME(detected) {
			return "", fmt.Errorf("unsupported image type %q; document references require MIME type", detected)
		}
		if mt != "" && mt != detected {
			return "", errors.New("binary file MIME type differs from original image")
		}
		mt = detected
	}
	if !cached {
		if err := cache.Put(ctx, "original", f.SHA256, data); err != nil {
			return "", fmt.Errorf("cache reconstructed binary file: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func (f BinaryFile) sourceBytes(ctx context.Context) ([]byte, error) {
	// Validate the opened descriptor rather than a racy pathname stat.
	source, err := os.OpenFile(f.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	stop := context.AfterFunc(ctx, func() { _ = source.Close() })
	defer stop()
	st, err := source.Stat()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("source is not a regular file")
	}
	if st.Size() > maxBinaryFileBytes {
		return nil, fmt.Errorf("source exceeds %d bytes", maxBinaryFileBytes)
	}
	data, err := io.ReadAll(binaryContextReader{ctx: ctx, r: io.LimitReader(source, maxBinaryFileBytes+1)})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if len(data) > maxBinaryFileBytes {
		return nil, fmt.Errorf("source exceeds %d bytes", maxBinaryFileBytes)
	}
	checksum := sha256.Sum256(data)
	if hex.EncodeToString(checksum[:]) != f.SHA256 {
		return nil, errors.New("source SHA-256 checksum mismatch; original binary file has changed")
	}
	return data, nil
}

type binaryContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r binaryContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	return r.r.Read(p)
}
