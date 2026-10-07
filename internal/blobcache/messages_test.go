package blobcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ttc/internal/prompts"
)

// Guard asset bytes independently of their generated constants and callers.
func TestDiagnosticAssetsExact(t *testing.T) {
	for _, tc := range []struct {
		name, got, want string
	}{
		{"miss", prompts.BlobCacheMiss, "blob cache miss"},
		{"absolute root", prompts.BlobCacheRootAbsolute, "blob cache root must be absolute"},
		{"directory root", prompts.BlobCacheRootDirectory, "blob cache root is not a directory"},
		{"invalid lock", prompts.BlobCacheInvalidLock, "invalid cache lock: %w"},
		{"kind", prompts.BlobCacheInvalidKind, "invalid blob kind %q"},
		{"key", prompts.BlobCacheInvalidKey, "invalid blob key"},
		{"regular entry", prompts.BlobCacheEntryRegular, "blob cache entry is not a regular file"},
		{"hard links", prompts.BlobCacheEntryHardLinks, "blob cache entry has multiple hard links"},
		{"configuration", prompts.BlobCacheInvalidConfiguration, "invalid blob cache configuration"},
		{"entry size", prompts.BlobCacheEntryTooLarge, "%s exceeds %d bytes"},
		{"reference", prompts.BlobCacheCorruptReference, "corrupt cache reference %s"},
		{"checksum", prompts.BlobCacheChecksumMismatch, "cache content checksum mismatch: %s"},
		{"expired miss", prompts.BlobCacheExpiredMiss, "%w: expired %s"},
		{"blob size", prompts.BlobCacheBlobTooLarge, "blob exceeds %d bytes"},
		{"original checksum", prompts.BlobCacheOriginalChecksum, "original key does not match content SHA256"},
		{"lock size", prompts.BlobCacheLockEmpty, "cache lock must be empty"},
		{"prune blob size", prompts.BlobCachePruneBlobTooLarge, "%s: blob exceeds %d bytes"},
		{"reservation", prompts.BlobCacheReserveSpace, "cache cannot reserve space within total limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("diagnostic changed: %q, want %q", tc.got, tc.want)
			}
		})
	}
}

func TestRuntimeDiagnosticsExact(t *testing.T) {
	ctx := context.Background()
	c := openCache(t)
	k := key(1)
	_, rootErr := New("relative")
	_, kindErr := c.Get(ctx, "other", k)
	_, keyErr := c.Get(ctx, "original", "bad")
	_, missErr := c.Get(ctx, "original", k)
	originalErr := c.Put(ctx, "original", k, []byte("data"))
	bad := &Cache{Root: "relative", Limit: c.Limit, TTL: c.TTL}
	_, configErr := bad.Get(ctx, "original", k)
	for _, tc := range []struct {
		err  error
		want string
	}{
		{rootErr, "blob cache root must be absolute"},
		{kindErr, `invalid blob kind "other"`},
		{keyErr, "invalid blob key"},
		{missErr, "blob cache miss: original " + k},
		{originalErr, "original key does not match content SHA256"},
		{configErr, "invalid blob cache configuration"},
	} {
		if tc.err == nil || tc.err.Error() != tc.want {
			t.Fatalf("diagnostic: %v, want %q", tc.err, tc.want)
		}
	}
	if !errors.Is(missErr, ErrMiss) {
		t.Fatal("miss lost its sentinel identity")
	}
	data := []byte("data")
	sha := digest(data)
	put(t, c, "original", sha, data)
	age(t, filepath.Join(c.Root, blobName(sha)), 31*24*time.Hour)
	_, err := c.Get(ctx, "original", sha)
	if want := "blob cache miss: expired " + blobName(sha); err == nil || err.Error() != want || !errors.Is(err, ErrMiss) {
		t.Fatalf("expired miss: %v, want %q with ErrMiss identity", err, want)
	}
	put(t, c, "render", k, data)
	if err := os.WriteFile(filepath.Join(c.Root, refName(k)), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(ctx, "render", k)
	if want := "corrupt cache reference " + refName(k); err == nil || err.Error() != want {
		t.Fatalf("corrupt reference: %v, want %q", err, want)
	}
	if err := os.WriteFile(filepath.Join(c.Root, blobName(sha)), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(ctx, "original", sha)
	if want := "cache content checksum mismatch: " + blobName(sha); err == nil || err.Error() != want {
		t.Fatalf("checksum: %v, want %q", err, want)
	}
	// Wrapping still exposes the original cause, not just its rendered text.
	cause := errors.New("cause")
	err = fmt.Errorf(prompts.BlobCacheInvalidLock, cause)
	if err.Error() != "invalid cache lock: cause" || !errors.Is(err, cause) {
		t.Fatalf("lock wrapping changed: %v", err)
	}
}
