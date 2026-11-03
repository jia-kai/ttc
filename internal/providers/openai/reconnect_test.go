package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	flag "github.com/spf13/pflag"

	"ttc/internal/llm"
	"ttc/internal/providerhttp"
	"ttc/internal/providers"
)

type reconnectTestTransport struct {
	roundTrip func(*http.Request) (*http.Response, error)
	closed    atomic.Int32
}

func (t *reconnectTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.roundTrip(req)
}
func (t *reconnectTestTransport) CloseIdleConnections() { t.closed.Add(1) }

type reconnectStalledBody struct {
	ctx     context.Context
	prefix  *strings.Reader
	started chan struct{}
	once    sync.Once
}

func (b *reconnectStalledBody) Read(p []byte) (int, error) {
	if b.prefix.Len() != 0 {
		return b.prefix.Read(p)
	}
	b.once.Do(func() { close(b.started) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (*reconnectStalledBody) Close() error { return nil }

func TestReconnectDuringValidatedCompletionPreservesConsumerOutcome(t *testing.T) {
	consumerFailure := errors.New("synthetic consumer failure")
	for _, kind := range []string{"state", "completed"} {
		for _, consumerErr := range []error{nil, consumerFailure} {
			t.Run(fmt.Sprintf("%s/failure=%t", kind, consumerErr != nil), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					started, release := make(chan struct{}), make(chan struct{})
					requests := 0
					base := &reconnectTestTransport{roundTrip: func(*http.Request) (*http.Response, error) {
						requests++
						return idleTestResponse(io.NopCloser(strings.NewReader(sseFrames(
							messageDone(0, ""), map[string]any{"type": "response.completed"})))), nil
					}}
					client, reconnect := providerhttp.New(&http.Client{Transport: base})
					defer reconnect.Close()
					a := idleStreamAdapter(client.Transport)
					a.Client = client
					done := make(chan error, 1)
					go func() {
						done <- a.Stream(context.Background(), partialRequest(0, 2), func(event llm.StreamEvent) error {
							if event.Kind == kind {
								close(started)
								<-release
								return consumerErr
							}
							return nil
						})
					}()
					<-started
					if err := reconnect.Reset(); err != nil {
						t.Fatal(err)
					}
					close(release)
					if err := <-done; !errors.Is(err, consumerErr) || requests != 1 {
						t.Fatal("reset replaced validated completion or local failure", err, requests)
					}
				})
			})
		}
	}
}

func TestReconnectRepeatedActiveStandardConnectionsClose(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%t", http2), func(t *testing.T) {
			const attempts = 5
			closed := make(chan struct{}, attempts)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-req.Context().Done()
			}))
			server.EnableHTTP2 = http2
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateClosed {
					closed <- struct{}{}
				}
			}
			if http2 {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			source := server.Client()
			// Do not let an idle timeout hide an incompletely retired HTTP/2 pool.
			source.Transport.(*http.Transport).IdleConnTimeout = 0
			client, reconnect := providerhttp.New(source)
			defer reconnect.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for range attempts {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				if http2 && resp.ProtoMajor != 2 {
					t.Fatal("fixture did not negotiate HTTP/2")
				}
				done := make(chan error, 1)
				go func() { _, err := io.ReadAll(resp.Body); resp.Body.Close(); done <- err }()
				if err := reconnect.Reset(); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if !errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, context.Canceled) {
						t.Fatal("reset transport failure misclassified", err)
					}
				case <-ctx.Done():
					t.Fatal("active reset did not unblock response body", ctx.Err())
				}
				select {
				case <-closed:
				case <-ctx.Done():
					t.Fatal("retired connection remained open", ctx.Err())
				}
			}
			if ctx.Err() != nil {
				t.Fatal("reset waited for watchdog", ctx.Err())
			}
		})
	}
}

func TestReconnectResetsStreamsWithinExistingRecoveryPolicy(t *testing.T) {
	for _, mode := range []string{"headers", "body", "partial", "no-tools"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := make(chan struct{})
				requests := 0
				base := &reconnectTestTransport{roundTrip: func(req *http.Request) (*http.Response, error) {
					requests++
					if requests > 1 {
						return idleTestResponse(io.NopCloser(strings.NewReader(sseFrames(
							map[string]any{"type": "response.output_text.delta", "delta": "done"}, messageDone(0, "done"), map[string]any{"type": "response.completed"})))), nil
					}
					if mode == "headers" {
						close(started)
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					prefix := ""
					if mode == "partial" || mode == "no-tools" {
						prefix = sseFrames(partialOutput("text"))
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Retry-After": {"0"}}, Body: &reconnectStalledBody{
						ctx: req.Context(), prefix: strings.NewReader(prefix), started: started,
					}}, nil
				}}
				client, reconnect := providerhttp.New(&http.Client{Transport: base})
				defer reconnect.Close()
				a := idleStreamAdapter(client.Transport)
				a.Client, a.idleTimeout = client, 0
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				req := partialRequest(0, 2)
				req.NoTools = mode == "no-tools"
				text, retries, completions := "", 0, 0
				done := make(chan error, 1)
				go func() {
					done <- a.Stream(ctx, req, func(event llm.StreamEvent) error {
						text += event.Text
						if event.Kind == "retry" {
							retries++
						}
						if event.Kind == "completed" {
							completions++
						}
						return nil
					})
				}()
				<-started
				start := time.Now()
				if err := reconnect.Reset(); err != nil {
					t.Fatal(err)
				}
				err := <-done
				var partial *llm.PartialError
				if mode == "partial" {
					if !errors.As(err, &partial) || requests != 1 || retries != 0 || completions != 0 || text != "partial" {
						t.Fatal("reset replayed committed output", err, requests, retries, completions, text)
					}
				} else if err != nil || requests != 2 || retries != 1 || completions != 1 || text != "done" {
					t.Fatal("reset bypassed recovery policy", err, requests, retries, completions, text)
				}
				if ctx.Err() != nil || errors.Is(err, context.Canceled) || time.Since(start) > 2*time.Second {
					t.Fatal("reset canceled caller or waited for watchdog", err, ctx.Err(), time.Since(start))
				}
				// Reset is repeatable and leaves a future request usable.
				if err := reconnect.Reset(); err != nil {
					t.Fatal(err)
				}
				if err := a.Stream(ctx, partialRequest(0, 1), func(llm.StreamEvent) error { return nil }); err != nil {
					t.Fatal("reset poisoned future request", err)
				}
				if base.closed.Load() != 2 {
					t.Fatal("custom idle pool reset count", base.closed.Load())
				}
			})
		})
	}
}

func TestReconnectUsesFreshStandardPoolAfterRouteChange(t *testing.T) {
	oldServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "old") }))
	defer oldServer.Close()
	newServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "new") }))
	defer newServer.Close()
	var route atomic.Value
	route.Store(strings.TrimPrefix(oldServer.URL, "http://"))
	base := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, route.Load().(string))
	}}
	defer base.CloseIdleConnections()
	original := &http.Client{Transport: base, Timeout: 3 * time.Second}
	client, reconnect := providerhttp.New(original)
	defer reconnect.Close()
	fetch := func(client *http.Client) string {
		t.Helper()
		resp, err := client.Get("http://route.test/response")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if fetch(original) != "old" || fetch(client) != "old" {
		t.Fatal("old route was not established")
	}
	route.Store(strings.TrimPrefix(newServer.URL, "http://"))
	if fetch(client) != "old" {
		t.Fatal("fixture did not retain a reusable old-route connection")
	}
	if err := reconnect.Reset(); err != nil || fetch(client) != "new" {
		t.Fatal("reset reused the old route", err)
	}
	if original.Transport != base || fetch(original) != "old" {
		t.Fatal("reset mutated or closed caller-owned standard pool")
	}
}

func TestModuleReconnectCoversAuthBoundCatalogAndInference(t *testing.T) {
	started := make(chan string, 3)
	var mu sync.Mutex
	requests := make(map[string]int)
	base := &reconnectTestTransport{roundTrip: func(req *http.Request) (*http.Response, error) {
		path := req.URL.Path
		mu.Lock()
		requests[path]++
		attempt := requests[path]
		mu.Unlock()
		if attempt == 1 {
			started <- path
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		switch path {
		case "/models":
			return idleTestResponse(io.NopCloser(strings.NewReader(`{"models":[{"slug":"tiny","visibility":"list","context_window":100}]}`))), nil
		case "/responses":
			return idleTestResponse(io.NopCloser(strings.NewReader(sseFrames(map[string]any{"type": "response.completed"})))), nil
		default:
			return nil, errors.New("unexpected synthetic request")
		}
	}}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	configured, err := providers.Configure([]providers.Module{Module()}, fs)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Parse([]string{"--openai-base-url", "https://mock.test", "--import-codex-auth", credentialFile(t, "synthetic-account")}); err != nil {
		t.Fatal(err)
	}
	components, err := configured[0].Open(context.Background(), providers.Environment{
		DataDir: filepath.Join(t.TempDir(), "provider"), Client: &http.Client{Transport: base},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer components.Connections.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	binding, err := components.Catalog.Bind(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waitStarted := func(want string) {
		t.Helper()
		select {
		case got := <-started:
			if got != want {
				t.Fatalf("started %q, want %q", got, want)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	catalogDone, inferenceDone, authDone := make(chan error, 1), make(chan error, 1), make(chan error, 1)
	go func() { _, err := binding.Source.Models(ctx); catalogDone <- err }()
	waitStarted("/models")
	go func() {
		inferenceDone <- components.Inference.Stream(ctx, partialRequest(0, 2), func(llm.StreamEvent) error { return nil })
	}()
	waitStarted("/responses")
	go func() { authDone <- components.Authorize(ctx, &testLogin{}) }()
	waitStarted("/api/accounts/deviceauth/usercode")
	if err := components.Connections.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := <-authDone; err == nil || errors.Is(err, context.Canceled) {
		t.Fatal("reset did not terminate auth exchange as transport failure", err)
	}
	if err := <-catalogDone; err != nil {
		t.Fatal("bound catalog failed to recover", err)
	}
	if err := <-inferenceDone; err != nil || ctx.Err() != nil {
		t.Fatal("shared inference failed to recover", err, ctx.Err())
	}
	if base.closed.Load() != 1 {
		t.Fatal("shared custom pool was not closed once", base.closed.Load())
	}
}

func TestReconnectCatalogBodyFailureRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		requests := 0
		base := &reconnectTestTransport{roundTrip: func(req *http.Request) (*http.Response, error) {
			requests++
			if requests == 1 {
				return &http.Response{StatusCode: 200, Header: http.Header{"Retry-After": {"0"}}, Body: &reconnectStalledBody{
					ctx: req.Context(), prefix: strings.NewReader(""), started: started,
				}}, nil
			}
			return idleTestResponse(io.NopCloser(strings.NewReader(`{"models":[{"slug":"tiny","visibility":"list","context_window":100}]}`))), nil
		}}
		client, reconnect := providerhttp.New(&http.Client{Transport: base})
		defer reconnect.Close()
		a := idleStreamAdapter(client.Transport)
		a.Client = client
		done := make(chan error, 1)
		go func() { _, err := a.Models(context.Background()); done <- err }()
		<-started
		if err := reconnect.Reset(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil || requests != 2 {
			t.Fatal("reset catalog body did not retry", err, requests)
		}
	})
}

func TestCatalogCloseDuringBodyPreservesPermanentCategory(t *testing.T) {
	for _, readErr := range []error{syscall.ECONNABORTED, &idleHTTPError{interval: time.Second}} {
		t.Run(fmt.Sprintf("%T", readErr), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var manager *providerhttp.Manager
				requests := 0
				base := retryTransport(func(req *http.Request) (*http.Response, error) {
					requests++
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Retry-After": {"30"}},
						Request:    req,
						Body: idleTestBody{read: func([]byte) (int, error) {
							if err := manager.Close(); err != nil {
								t.Fatal(err)
							}
							return 0, readErr
						}},
					}, nil
				})
				client, manager := providerhttp.New(&http.Client{Transport: base})
				defer manager.Close()
				adapter := idleStreamAdapter(client.Transport)
				adapter.Client = client
				start := time.Now()
				_, err := adapter.Models(context.Background())
				if !errors.Is(err, providerhttp.ErrClosed) || time.Since(start) != 0 || requests != 1 {
					t.Fatal("permanent Close entered catalog retry backoff", err, time.Since(start), requests)
				}
			})
		})
	}
}

func TestModuleCloseRejectsAuthCatalogAndInferenceWithoutRetry(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	configured, err := providers.Configure([]providers.Module{Module()}, fs)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Parse([]string{"--openai-base-url", "https://mock.test", "--import-codex-auth", credentialFile(t, "synthetic-account")}); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	base := &reconnectTestTransport{roundTrip: func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("request escaped closed manager")
	}}
	components, err := configured[0].Open(context.Background(), providers.Environment{
		DataDir: filepath.Join(t.TempDir(), "provider"), Client: &http.Client{Transport: base},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer components.Connections.Close()
	binding, err := components.Catalog.Bind(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if binding.Source.(*Adapter).Client != components.Inference.(*Adapter).Client {
		t.Fatal("bound catalog did not retain shared provider client")
	}
	if err := components.Connections.Close(); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		if err := components.Authorize(context.Background(), &testLogin{}); !errors.Is(err, providerhttp.ErrClosed) {
			t.Fatal("authorization lost closed ownership", err)
		}
		if _, err := binding.Source.Models(context.Background()); !errors.Is(err, providerhttp.ErrClosed) {
			t.Fatal("catalog retried closed ownership", err)
		}
		for _, noTools := range []bool{false, true} {
			req := partialRequest(0, 3)
			req.NoTools = noTools
			if err := components.Inference.Stream(context.Background(), req, func(event llm.StreamEvent) error {
				if event.Kind == "retry" {
					t.Error("inference retried permanent close")
				}
				return nil
			}); !errors.Is(err, providerhttp.ErrClosed) {
				t.Fatal("inference lost closed ownership", err)
			}
		}
		if !time.Now().Equal(start) || requests.Load() != 0 {
			t.Fatal("closed manager waited or admitted requests", time.Since(start), requests.Load())
		}
	})
}

func TestClosedConnectionsDoNotRetryExpiredTokenRefresh(t *testing.T) {
	owner := expiredUpstreamAuth(t, func(http.ResponseWriter, *http.Request) {
		t.Error("token refresh escaped closed manager")
	})
	client, manager := providerhttp.New(owner.Client)
	owner.Client = client
	adapter := transportAdapter(owner)
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		if _, err := owner.AccessTokens(context.Background()); !errors.Is(err, providerhttp.ErrClosed) {
			t.Fatal("refresh lost permanent close", err)
		}
		if _, err := adapter.Models(context.Background()); !errors.Is(err, providerhttp.ErrClosed) {
			t.Fatal("catalog retried closed token acquisition", err)
		}
		err := adapter.Stream(context.Background(), partialRequest(0, 0), func(event llm.StreamEvent) error {
			if event.Kind == "retry" {
				t.Error("token acquisition spent stream retry budget after Close")
			}
			return nil
		})
		var transient *llm.TransientError
		if !errors.Is(err, providerhttp.ErrClosed) || errors.As(err, &transient) || !time.Now().Equal(start) {
			t.Fatal("closed refresh classified as retryable upstream failure", err, time.Since(start))
		}
	})
}
