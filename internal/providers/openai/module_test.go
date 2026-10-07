package openai

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ttc/internal/filelock"
	"ttc/internal/providers"
)

func credentialFile(t *testing.T, account string) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "codex.json")
	raw, err := json.Marshal(Credentials{AuthMode: "chatgpt", Tokens: CredentialsTokens{Access: "synthetic", AccountID: account}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return source
}

func importTestCredentials(t *testing.T, owner *Authenticator, account string) {
	t.Helper()
	if err := owner.ImportCodex(context.Background(), credentialFile(t, account)); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAICatalogBindingCapturesIdentityAndGuardsCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "openai-auth.json")
	owner := NewAuthenticator(path)
	importTestCredentials(t, owner, "first")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "second" {
			t.Errorf("wrong account %q", got)
		}
		fmt.Fprint(w, `{"models":[{"slug":"second-model","display_name":"Second","visibility":"list","context_window":100000,"default_reasoning_level":"none","supported_reasoning_levels":[{"effort":"none"}]}]}`)
	}))
	defer server.Close()
	transport := NewAdapter(Config{Client: server.Client(), BaseURL: server.URL, TokenSource: owner.AccessTokens})
	old, err := bindCatalog(context.Background(), owner, transport)
	if err != nil {
		t.Fatal(err)
	}
	if old.Scope.Provider != "openai" || old.Scope.Endpoint != server.URL || old.Scope.Account != "first" || old.Scope.Version != CatalogVersion || calls.Load() != 0 {
		t.Fatal(old.Scope, calls.Load())
	}
	importTestCredentials(t, owner, "second")
	if _, err := old.Source.Models(context.Background()); err == nil || !strings.Contains(err.Error(), "account changed") {
		t.Fatal("stale source accepted credentials", err)
	}
	committed := false
	if err := old.Guard(context.Background(), func() error { committed = true; return nil }); err == nil || committed || calls.Load() != 0 {
		t.Fatal("stale binding committed or requested", err, committed, calls.Load())
	}
	current, err := bindCatalog(context.Background(), owner, transport)
	if err != nil {
		t.Fatal(err)
	}
	models, err := current.Source.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "second-model" {
		t.Fatal(models, err)
	}
	if err := current.Validate(models); err != nil {
		t.Fatal(err)
	}
	// The binding delegates the cross-process credential writer lock.
	lock, err := filelock.Acquire(context.Background(), path+".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := current.Guard(ctx, func() error { committed = true; return nil }); !errors.Is(err, context.DeadlineExceeded) || committed {
		t.Fatal(err, committed)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := current.Guard(context.Background(), func() error { committed = true; return nil }); err != nil || !committed {
		t.Fatal(err, committed)
	}
}

func TestOpenAIConstruction(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	configured, err := providers.Configure([]providers.Module{Module()}, fs)
	if err != nil {
		t.Fatal(err)
	}
	source := credentialFile(t, "imported")
	if err := fs.Parse([]string{"--openai-base-url", "http://example.invalid", "--import-codex-auth", source}); err != nil {
		t.Fatal(err)
	}
	factory, err := providers.Select(configured, "openai")
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(t.TempDir(), "data")
	client := &http.Client{}
	components, err := factory(context.Background(), providers.Environment{DataDir: data, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	if components.Inference == nil || components.Catalog == nil || components.Authorize == nil || components.Notice == "" {
		t.Fatal(components)
	}
	adapter := components.Inference.(*Adapter)
	if adapter.Client != client || adapter.BaseURL != "http://example.invalid" {
		t.Fatal(adapter)
	}
	if components.Catalog.CachePath != filepath.Join(data, "openai-models.json") {
		t.Fatal(components.Catalog.CachePath)
	}
	if _, err := os.Stat(filepath.Join(data, "openai-auth.json")); err != nil {
		t.Fatal(err)
	}
	binding, err := components.Catalog.Bind(context.Background())
	if err != nil || binding.Scope.Account != "imported" {
		t.Fatal(binding, err)
	}
	if _, err := os.Stat(components.Catalog.CachePath); !os.IsNotExist(err) {
		t.Fatal("construction created a cache", err)
	}
}

func TestOpenAIConstructionValidationAndNoEffects(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	configured, err := providers.Configure([]providers.Module{Module()}, fs)
	if err != nil {
		t.Fatal(err)
	}
	factory := configured[0].Open
	if _, err := factory(context.Background(), providers.Environment{}); err == nil {
		t.Fatal("accepted empty environment")
	}
	data := filepath.Join(t.TempDir(), "not-created")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := factory(ctx, providers.Environment{DataDir: data}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := factory(nil, providers.Environment{DataDir: data}); err == nil {
		t.Fatal("accepted nil context")
	}
	components, err := factory(context.Background(), providers.Environment{DataDir: data})
	if err != nil || components.Notice != "" {
		t.Fatal(components, err)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("construction touched data directory", err)
	}
	if err := fs.Parse([]string{"--import-codex-auth", filepath.Join(t.TempDir(), "missing")}); err != nil {
		t.Fatal(err)
	}
	if _, err := factory(ctx, providers.Environment{DataDir: data}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := factory(context.Background(), providers.Environment{DataDir: data}); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("invalid import touched data directory", err)
	}
}
