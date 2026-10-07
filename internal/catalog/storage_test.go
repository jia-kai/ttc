package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"ttc/internal/llm"
)

func privateTempDir(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func putRecord(t *testing.T, path string, c cacheRecord) {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestPrivateNeutralCacheAndScopes(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "models.json")
	seed(t, path)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"https://private.example", "private-account", "access_token", "refresh_token", "budget", "output_allowance"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("cache contains raw private identity or policy %q", private)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	b := binding(nil)
	for _, kind := range []string{"provider", "endpoint", "account", "format", "schema", "old-schema"} {
		t.Run(kind, func(t *testing.T) {
			scope := b.Scope
			c := record(scope, model("cached"))
			switch kind {
			case "provider":
				scope.Provider = "other"
			case "endpoint":
				scope.Endpoint += "/other"
			case "account":
				scope.Account = "other"
			case "format":
				scope.Version = "v2"
			case "schema":
				c.Version++
			case "old-schema":
				c.Version = 1
				c.Provider = ""
				c.CatalogVersion = ""
			}
			putRecord(t, path, c)
			changed := b
			changed.Scope = scope
			if models, err := readCache(context.Background(), path, changed); err != nil || models != nil {
				t.Fatal(models, err)
			}
		})
	}
}
func TestCorruptCacheWarnsAndRefetches(t *testing.T) {
	for _, kind := range []string{"json", "trailing", "empty-envelope", "utf8", "oversized", "empty", "duplicate", "capacity", "default", "variant", "description", "scope"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(privateTempDir(t), "models.json")
			b := binding(nil)
			c := record(b.Scope, model("cached"))
			var raw []byte
			switch kind {
			case "json":
				raw = []byte("{")
			case "trailing":
				raw = []byte("{} {}")
			case "empty-envelope":
				raw = []byte("{}")
			case "utf8":
				raw = []byte{0xff}
			case "oversized":
				raw = []byte(strings.Repeat(" ", maxCacheBytes+1))
			case "empty":
				c.Models = nil
			case "duplicate":
				c.Models = append(c.Models, c.Models[0])
			case "capacity":
				c.Models[0].Limits.ContextLimit = 0
			case "default":
				c.Models[0].DefaultVariant = "unknown"
			case "variant":
				c.Models[0].Variants = []string{"none", "none"}
			case "description":
				c.Models[0].VariantDescriptions["unknown"] = "bad"
			case "scope":
				c.EndpointHash = "bad"
			}
			if raw != nil {
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				putRecord(t, path, c)
			}
			if models, err := readCache(context.Background(), path, b); models != nil || !errors.Is(err, ErrInvalidCache) {
				t.Fatal(models, err)
			}
			m, err := Open(context.Background(), config(path, sourceFunc(func(context.Context) ([]llm.ModelInfo, error) { return model("fresh"), nil })))
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if !strings.Contains(m.Notice, "Ignoring invalid model catalog cache") || m.Initial[0].ID != "fresh" {
				t.Fatal(m.Notice, m.Initial)
			}
		})
	}
}
func TestCacheOperationalErrors(t *testing.T) {
	for _, kind := range []string{"mode", "directory", "symlink", "fifo", "parent-file"} {
		t.Run(kind, func(t *testing.T) {
			dir := privateTempDir(t)
			path := filepath.Join(dir, "models.json")
			var err error
			switch kind {
			case "mode":
				err = os.WriteFile(path, []byte("{}"), 0644)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "symlink":
				err = os.Symlink(filepath.Join(dir, "missing"), path)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			case "parent-file":
				err = os.WriteFile(path, []byte("x"), 0600)
				path = filepath.Join(path, "child")
			}
			if err != nil {
				t.Fatal(err)
			}
			called := false
			m, err := Open(context.Background(), config(path, sourceFunc(func(context.Context) ([]llm.ModelInfo, error) { called = true; return model("fresh"), nil })))
			if m != nil || err == nil || errors.Is(err, ErrInvalidCache) || called {
				t.Fatal(m, err, called)
			}
		})
	}
}
func TestCacheSaveFailureAndSymlinkReplacement(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "models.json")
	b := binding(nil)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := saveCache(context.Background(), path, b, model("new")); err == nil {
		t.Fatal("directory replacement succeeded")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "untouched")
	if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := saveCache(context.Background(), path, b, model("new")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != "untouched" {
		t.Fatal(string(raw), err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ttc-") {
			t.Fatal("temporary file leaked", e.Name())
		}
	}
}
func TestGenericTextValidationAndInjectedValidation(t *testing.T) {
	for _, field := range []string{"id", "variant"} {
		limit := llm.MaxModelIDBytes
		if field == "variant" {
			limit = llm.MaxVariantBytes
		}
		for _, text := range []string{"", " padded", "padded ", "control\x00", "control\u0085", string([]byte{0xff}), strings.Repeat("a", limit+1)} {
			models := model("valid")
			if field == "id" {
				models[0].ID = text
			} else {
				models[0].Variants = []string{text}
				models[0].DefaultVariant = text
			}
			if err := validate(models, nil); err == nil {
				t.Fatalf("invalid %s %q accepted", field, text)
			}
		}
	}
	failure := errors.New("provider-specific constraint")
	b := binding(nil)
	b.Validate = func([]llm.ModelInfo) error { return failure }
	path := filepath.Join(privateTempDir(t), "models.json")
	putRecord(t, path, record(b.Scope, model("cached")))
	if _, err := readCache(context.Background(), path, b); !errors.Is(err, ErrInvalidCache) {
		t.Fatal(err)
	}
	c := config("", sourceFunc(func(context.Context) ([]llm.ModelInfo, error) { return model("fresh"), nil }))
	c.Bind = func(context.Context) (Binding, error) {
		b.Source = sourceFunc(func(context.Context) ([]llm.ModelInfo, error) { return model("fresh"), nil })
		return b, nil
	}
	if _, err := Open(context.Background(), c); !errors.Is(err, failure) {
		t.Fatal(err)
	}
}
func TestCancellationAndConfigValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readCache(ctx, "", binding(nil)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := saveCache(ctx, "", binding(nil), model("x")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	c := config("", sourceFunc(func(context.Context) ([]llm.ModelInfo, error) { return model("fresh"), nil }))
	c.Timeout = -1
	if _, err := Open(context.Background(), c); err == nil {
		t.Fatal("negative timeout accepted")
	}
	c.Timeout = 0
	c.Policy = Policy{OutputAllowance: 1}
	if _, err := Open(context.Background(), c); err == nil {
		t.Fatal("invalid policy accepted")
	}
}
func TestPolicyCapacitiesAndSnapshotOwnership(t *testing.T) {
	raw := model("large")
	raw[0].BinaryFiles = []llm.BinaryFileType{{MIMEType: "application/pdf", Extensions: []string{".pdf"}, Kind: "document", MaxBytes: 1024}}
	small := model("small")[0]
	small.Limits.ContextLimit = 1
	raw = append(raw, small)
	raw[0].Limits.MaxOutputTokens = 100
	specs, err := prepare(raw, DefaultPolicy())
	if err != nil || len(specs) != 1 {
		t.Fatal(specs, err)
	}
	if specs[0].Budget.OutputAllowance != 100 || specs[0].Budget.SummaryOutputAllowance != 100 {
		t.Fatal(specs[0].Budget)
	}
	raw[0].Variants[0] = "mutated"
	raw[0].VariantDescriptions["none"] = "mutated"
	raw[0].BinaryFiles[0].Extensions[0] = ".mutated"
	if specs[0].BinaryFiles[0].Extensions[0] != ".pdf" {
		t.Fatal("snapshot aliases binary extension slices")
	}
	if specs[0].Variants[0] != "none" || specs[0].VariantDescriptions["none"] != "No reasoning" {
		t.Fatal("snapshot aliases source")
	}
	if _, err := prepare([]llm.ModelInfo{small}, DefaultPolicy()); err == nil {
		t.Fatal("all unusable capacities accepted")
	}
	overflow := DefaultPolicy()
	overflow.OutputAllowance = int(^uint(0) >> 1)
	if _, err := prepare(model("x"), overflow); err == nil {
		t.Fatal("overflow reserves accepted")
	}
}
