package openai

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"ttc/internal/catalog"
	"ttc/internal/filelock"
	"ttc/internal/providers"
)

func TestWarmCatalogIdentityLockWaitIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "openai-auth.json")
	owner := NewAuthenticator(path)
	importTestCredentials(t, owner, "account")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"models":[{"slug":"model","visibility":"list","context_window":100000}]}`)
	}))
	defer server.Close()
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	configured, err := providers.Configure([]providers.Module{Module()}, flags)
	if err != nil {
		t.Fatal(err)
	}
	if err := flags.Parse([]string{"--openai-base-url", server.URL}); err != nil {
		t.Fatal(err)
	}
	open, err := providers.Select(configured, "openai")
	if err != nil {
		t.Fatal(err)
	}
	components, err := open(context.Background(), providers.Environment{DataDir: filepath.Dir(path), Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	config := catalog.Config{Bind: components.Catalog.Bind, CachePath: components.Catalog.CachePath, Timeout: time.Second}
	manager, err := catalog.Open(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	manager.Close()
	// Simulate another process's token refresh holding the shared writer lock.
	// Warm startup must not publish without validating the current account.
	lock, err := filelock.Acquire(context.Background(), path+".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	config.Timeout = 30 * time.Millisecond
	start := time.Now()
	manager, err = catalog.Open(context.Background(), config)
	if manager != nil {
		manager.Close()
		t.Fatal("published while credential writer held lock")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("identity lock ignored startup deadline")
	}
	if calls.Load() != 1 {
		t.Fatal("warm startup performed catalog HTTP", calls.Load())
	}
}
