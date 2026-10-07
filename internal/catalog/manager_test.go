package catalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ttc/internal/llm"
)

type sourceFunc func(context.Context) ([]llm.ModelInfo, error)

func (f sourceFunc) Models(ctx context.Context) ([]llm.ModelInfo, error) { return f(ctx) }
func model(id string) []llm.ModelInfo {
	return []llm.ModelInfo{{ID: id, Name: id, Variants: []string{"none"}, DefaultVariant: "none", Limits: llm.ModelLimits{ContextLimit: 100000}, VariantDescriptions: map[string]string{"none": "No reasoning"}}}
}
func binding(s Source) Binding {
	return Binding{Scope: Scope{Provider: "second", Endpoint: "https://private.example", Account: "private-account", Version: "v1"}, Source: s}
}
func config(path string, s Source) Config {
	return Config{CachePath: path, Bind: func(context.Context) (Binding, error) { return binding(s), nil }, Timeout: time.Second}
}
func wait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not finish")
	}
}
func receive(t *testing.T, m *Manager) Update {
	t.Helper()
	select {
	case u := <-m.Updates:
		return u
	case <-time.After(3 * time.Second):
		t.Fatal("update not delivered")
		return Update{}
	}
}
func seed(t *testing.T, path string) {
	t.Helper()
	m, err := Open(context.Background(), config(path, sourceFunc(func(context.Context) ([]llm.ModelInfo, error) { return model("cached"), nil })))
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
}

func TestUncachedSecondProviderColdStartup(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	c := config("", sourceFunc(func(ctx context.Context) ([]llm.ModelInfo, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("missing deadline")
		}
		close(started)
		select {
		case <-release:
			return model("fresh"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}))
	done := make(chan struct{})
	var m *Manager
	var err error
	go func() { m, err = Open(context.Background(), c); close(done) }()
	wait(t, started)
	select {
	case <-done:
		t.Fatal("cold startup returned before fetch")
	default:
	}
	close(release)
	wait(t, done)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.Initial[0].ID != "fresh" || m.Notice != "" {
		t.Fatal(m.Initial, m.Notice)
	}
	select {
	case <-m.Updates:
		t.Fatal("cold startup delivered a background update")
	default:
	}
}
func TestWarmStartupSuccessFailureAndDeadline(t *testing.T) {
	for _, kind := range []string{"success", "failure", "deadline"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(privateTempDir(t), "models.json")
			seed(t, path)
			started, release := make(chan struct{}), make(chan struct{})
			failure := errors.New("unavailable")
			c := config(path, sourceFunc(func(ctx context.Context) ([]llm.ModelInfo, error) {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				if kind == "failure" {
					return nil, failure
				}
				return model("fresh"), nil
			}))
			if kind == "deadline" {
				c.Timeout = 20 * time.Millisecond
			}
			m, err := Open(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			wait(t, started)
			if m.Initial[0].ID != "cached" {
				t.Fatal(m.Initial)
			}
			if kind != "deadline" {
				close(release)
			}
			u := receive(t, m)
			switch kind {
			case "success":
				if u.Err != nil || u.Models[0].ID != "fresh" {
					t.Fatal(u)
				}
			case "failure":
				if !errors.Is(u.Err, failure) || u.Models != nil {
					t.Fatal(u)
				}
			case "deadline":
				if !errors.Is(u.Err, context.DeadlineExceeded) {
					t.Fatal(u)
				}
			}
			if m.Initial[0].ID != "cached" {
				t.Fatal("startup snapshot mutated")
			}
			select {
			case _, ok := <-m.Updates:
				if !ok {
					t.Fatal("worker closed updates")
				}
			default:
			}
		})
	}
}
func TestCloseCancelsAndJoinsBlockingHTTP(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "models.json")
	seed(t, path)
	started, finished := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(finished) }))
	defer server.Close()
	s := sourceFunc(func(ctx context.Context) ([]llm.ModelInfo, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		res, err := server.Client().Do(req)
		if res != nil {
			res.Body.Close()
		}
		return nil, err
	})
	m, err := Open(context.Background(), config(path, s))
	if err != nil {
		t.Fatal(err)
	}
	wait(t, started)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m.Close() }()
	}
	wg.Wait()
	wait(t, finished)
	if _, ok := <-m.Updates; ok {
		t.Fatal("updates not closed")
	}
}
func TestRefreshCancelsOldWorkerAndDrainsOldDelivery(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		t.Run(map[bool]string{false: "blocked", true: "buffered"}[buffered], func(t *testing.T) {
			path := filepath.Join(privateTempDir(t), "models.json")
			seed(t, path)
			started, finished := make(chan struct{}), make(chan struct{})
			old := sourceFunc(func(ctx context.Context) ([]llm.ModelInfo, error) {
				defer close(finished)
				close(started)
				if !buffered {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return model("stale"), nil
			})
			var mu sync.Mutex
			current := binding(old)
			c := config(path, old)
			c.Bind = func(context.Context) (Binding, error) { mu.Lock(); defer mu.Unlock(); return current, nil }
			m, err := Open(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			wait(t, started)
			if buffered {
				wait(t, finished)
				<-m.workerDone
			}
			m.Invalidate()
			wait(t, finished)
			select {
			case u := <-m.Updates:
				t.Fatal("stale update survived invalidation", u)
			default:
			}
			mu.Lock()
			current = binding(sourceFunc(func(context.Context) ([]llm.ModelInfo, error) { return model("current"), nil }))
			current.Scope.Account = "new-account"
			mu.Unlock()
			models, err := m.Refresh(context.Background())
			if err != nil || models[0].ID != "current" {
				t.Fatal(models, err)
			}
			wait(t, finished)
			select {
			case u := <-m.Updates:
				t.Fatal("stale update survived Refresh", u)
			default:
			}
			if m.Initial[0].ID != "cached" {
				t.Fatal("Refresh mutated Initial")
			}
			raw, err := readCache(context.Background(), path, current)
			if err != nil || raw[0].ID != "current" {
				t.Fatal(raw, err)
			}
		})
	}
}
func TestCloseCancelsSynchronousRefresh(t *testing.T) {
	started, finished := make(chan struct{}), make(chan struct{})
	var block bool
	c := config("", sourceFunc(func(ctx context.Context) ([]llm.ModelInfo, error) {
		if !block {
			return model("initial"), nil
		}
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	m, err := Open(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	block = true
	var refreshErr error
	go func() { _, refreshErr = m.Refresh(context.Background()); close(finished) }()
	wait(t, started)
	m.Close()
	wait(t, finished)
	if !errors.Is(refreshErr, context.Canceled) {
		t.Fatal(refreshErr)
	}
	if _, err := m.Refresh(context.Background()); err == nil {
		t.Fatal("refresh after close accepted")
	}
}

func TestRefreshWaitHonorsCancellationAndCloseJoins(t *testing.T) {
	for _, kind := range []string{"canceled", "deadline", "manager"} {
		t.Run(kind, func(t *testing.T) {
			root, cancelManager := context.WithCancel(context.Background())
			defer cancelManager()
			started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			c := config("", sourceFunc(func(ctx context.Context) ([]llm.ModelInfo, error) {
				if calls.Add(1) == 1 {
					return model("initial"), nil
				}
				close(started)
				<-ctx.Done()
				close(canceled)
				// Simulate source cleanup so shutdown must join, not just cancel.
				<-release
				return nil, ctx.Err()
			}))
			c.Timeout = 10 * time.Second
			m, err := Open(root, c)
			if err != nil {
				t.Fatal(err)
			}
			var releaseOnce sync.Once
			defer func() { releaseOnce.Do(func() { close(release) }); m.Close() }()
			first := make(chan error, 1)
			go func() { _, err := m.Refresh(context.Background()); first <- err }()
			wait(t, started)

			request, cancelRequest := context.WithCancel(context.Background())
			want := context.Canceled
			if kind == "deadline" {
				cancelRequest()
				request, cancelRequest = context.WithTimeout(context.Background(), 20*time.Millisecond)
				want = context.DeadlineExceeded
			}
			defer cancelRequest()
			second := make(chan error, 1)
			go func() { _, err := m.Refresh(request); second <- err }()
			switch kind {
			case "canceled":
				cancelRequest()
			case "manager":
				cancelManager()
			}
			select {
			case err := <-second:
				if !errors.Is(err, want) {
					t.Fatalf("waiting refresh: got %v, want %v", err, want)
				}
			case <-time.After(time.Second):
				t.Fatal("waiting refresh ignored cancellation")
			}
			if calls.Load() != 2 {
				t.Fatal("waiting refresh invoked the source")
			}
			select {
			case err := <-first:
				t.Fatal("first refresh finished before release", err)
			default:
			}

			closed := make(chan struct{})
			go func() { m.Close(); close(closed) }()
			wait(t, canceled)
			select {
			case <-closed:
				t.Fatal("Close returned before source cleanup finished")
			default:
			}
			joined := make(chan struct{})
			go func() { m.Invalidate(); m.Close(); close(joined) }()
			releaseOnce.Do(func() { close(release) })
			wait(t, closed)
			wait(t, joined)
			if err := <-first; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if _, ok := <-m.Updates; ok {
				t.Fatal("updates not closed")
			}
		})
	}
}

func TestGuardRejectsAccountReplacementWithoutCacheCommitOrPublication(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "models.json")
	seed(t, path)
	started, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	account := "private-account"
	s := sourceFunc(func(context.Context) ([]llm.ModelInfo, error) {
		close(started)
		<-release
		return model("stale"), nil
	})
	c := config(path, s)
	c.Bind = func(context.Context) (Binding, error) {
		b := binding(s)
		b.Guard = func(ctx context.Context, commit func() error) error {
			mu.Lock()
			defer mu.Unlock()
			if account != b.Scope.Account {
				return errors.New("account changed")
			}
			return commit()
		}
		return b, nil
	}
	m, err := Open(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	wait(t, started)
	mu.Lock()
	account = "other"
	mu.Unlock()
	close(release)
	u := receive(t, m)
	if u.Err == nil || u.Models != nil {
		t.Fatal(u)
	}
	raw, err := readCache(context.Background(), path, binding(nil))
	if err != nil || raw[0].ID != "cached" {
		t.Fatal("stale cache committed", raw, err)
	}
}
