package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"unicode/utf8"

	"ttc/internal/llm"
	"ttc/internal/privatefile"
)

const cacheVersion = 2
const maxCacheBytes = 4 << 20

// ErrInvalidCache identifies corrupt contents. Open warns and fetches a
// replacement; privacy and filesystem errors are always operational failures.
var ErrInvalidCache = errors.New("invalid model catalog cache")

type cacheRecord struct {
	Version        int             `json:"version"`
	Provider       string          `json:"provider"`
	CatalogVersion string          `json:"catalog_version"`
	EndpointHash   string          `json:"endpoint_hash"`
	AccountHash    string          `json:"account_hash"`
	Models         []llm.ModelInfo `json:"models"`
}

func scopeHash(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func record(scope Scope, models []llm.ModelInfo) cacheRecord {
	return cacheRecord{cacheVersion, scope.Provider, scope.Version, scopeHash(scope.Endpoint), scopeHash(scope.Account), models}
}
func validHash(s string) bool { b, e := hex.DecodeString(s); return e == nil && len(b) == sha256.Size }

func readCache(ctx context.Context, path string, b Binding) ([]llm.ModelInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open model catalog cache: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat model catalog cache: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.New("model catalog cache must be a private 0600 regular file")
	}
	if info.Size() > maxCacheBytes {
		return nil, fmt.Errorf("%w: exceeds 4 MiB", ErrInvalidCache)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxCacheBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read model catalog cache: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(raw) > maxCacheBytes {
		return nil, fmt.Errorf("%w: exceeds 4 MiB", ErrInvalidCache)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%w: require UTF-8 JSON", ErrInvalidCache)
	}
	var c cacheRecord
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%w: malformed JSON", ErrInvalidCache)
	}
	if c.Version <= 0 {
		return nil, fmt.Errorf("%w: missing schema version", ErrInvalidCache)
	}
	// Other schemas are incompatible misses, not migration inputs.
	if c.Version != cacheVersion {
		return nil, nil
	}
	if c.Provider == "" || c.CatalogVersion == "" || !validHash(c.EndpointHash) || !validHash(c.AccountHash) {
		return nil, fmt.Errorf("%w: incomplete scope", ErrInvalidCache)
	}
	expected := record(b.Scope, nil)
	if c.Provider != expected.Provider || c.CatalogVersion != expected.CatalogVersion || c.EndpointHash != expected.EndpointHash || c.AccountHash != expected.AccountHash {
		return nil, nil
	}
	if err := validate(c.Models, b.Validate); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCache, err)
	}
	return c.Models, nil
}

func saveCache(ctx context.Context, path string, b Binding, models []llm.ModelInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if path == "" {
		return nil
	}
	raw, err := json.Marshal(record(b.Scope, models))
	if err != nil {
		return fmt.Errorf("encode model catalog cache: %w", err)
	}
	if len(raw) > maxCacheBytes {
		return errors.New("model catalog cache exceeds 4 MiB")
	}
	if err := privatefile.PrivateDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("prepare model catalog cache directory: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := privatefile.AtomicFile(path, raw, 0600); err != nil {
		return fmt.Errorf("save model catalog cache: %w", err)
	}
	return ctx.Err()
}
