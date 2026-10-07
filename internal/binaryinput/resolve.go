package binaryinput

import (
	"context"
	"crypto/sha256"
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
	"ttc/internal/llm"
)

const maxBinaryFileBytes = 32 << 20

// Resolve retrieves checksum-verified original bytes cache-first. Reads are
// cancelable and bounded to 32 MiB; nonregular descriptors are rejected without
// blocking on FIFOs. Missing or changed sources return UnavailableBinaryFileError,
// while invalid references and operational cache failures are fatal. Documents
// require explicit MIME and byte size; image MIME can be detected from bytes.
// Pixel limits are checked before tool results or attachment snapshots are created.
func Resolve(ctx context.Context, f llm.BinaryFile) (llm.BinaryPayload, error) {
	if err := ctx.Err(); err != nil {
		return llm.BinaryPayload{}, err
	}
	if f.Bytes < 0 || f.Bytes > maxBinaryFileBytes {
		return llm.BinaryPayload{}, fmt.Errorf("binary file size must be between 0 and %d bytes", maxBinaryFileBytes)
	}
	if f.MIMEType != "" && !llm.ValidBinaryMIME(f.MIMEType) {
		return llm.BinaryPayload{}, errors.New("binary file requires a canonical MIME type without parameters")
	}
	if f.Path == "" || f.SHA256 == "" {
		return llm.BinaryPayload{}, errors.New("file-backed binary requires an absolute path and SHA-256 checksum")
	}
	if !filepath.IsAbs(f.Path) {
		return llm.BinaryPayload{}, fmt.Errorf("binary file path must be absolute: %q", f.Path)
	}
	if len(f.SHA256) != sha256.Size*2 || strings.Trim(f.SHA256, "0123456789abcdef") != "" {
		return llm.BinaryPayload{}, fmt.Errorf("binary file %q requires a lowercase hex SHA-256 checksum", f.Path)
	}
	if f.MIMEType != "" && !strings.HasPrefix(f.MIMEType, "image/") && f.Bytes == 0 {
		return llm.BinaryPayload{}, errors.New("file-backed document requires original byte size")
	}
	payload, err := resolveOriginal(ctx, f)
	if err != nil {
		return llm.BinaryPayload{}, fmt.Errorf("binary file %q: %w", f.Path, err)
	}
	return payload, nil
}

func resolveOriginal(ctx context.Context, f llm.BinaryFile) (llm.BinaryPayload, error) {
	cache, err := blobcache.Default()
	if err != nil {
		return llm.BinaryPayload{}, fmt.Errorf("open binary file cache: %w", err)
	}
	data, err := cache.Get(ctx, "original", f.SHA256)
	cached := err == nil
	if errors.Is(err, blobcache.ErrMiss) {
		data, err = sourceBytes(ctx, f)
		if err != nil {
			if ctx.Err() != nil {
				return llm.BinaryPayload{}, ctx.Err()
			}
			return llm.BinaryPayload{}, &llm.UnavailableBinaryFileError{File: f, Err: err}
		}
	} else if err != nil {
		return llm.BinaryPayload{}, fmt.Errorf("read cached binary file: %w", err)
	}
	if f.Bytes != 0 && f.Bytes != len(data) {
		return llm.BinaryPayload{}, errors.New("binary file byte size differs from original")
	}
	mt := f.MIMEType
	if mt == "" || strings.HasPrefix(mt, "image/") {
		detected := http.DetectContentType(data)
		if detected != "image/png" && detected != "image/jpeg" && detected != "image/gif" {
			return llm.BinaryPayload{}, fmt.Errorf("unsupported image type %q; document references require MIME type", detected)
		}
		if mt != "" && mt != detected {
			return llm.BinaryPayload{}, errors.New("binary file MIME type differs from original image")
		}
		mt = detected
	}
	if !cached {
		if err := cache.Put(ctx, "original", f.SHA256, data); err != nil {
			return llm.BinaryPayload{}, fmt.Errorf("cache reconstructed binary file: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return llm.BinaryPayload{}, err
	}
	return llm.BinaryPayload{Data: data, MIMEType: mt}, nil
}

func sourceBytes(ctx context.Context, f llm.BinaryFile) ([]byte, error) {
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
