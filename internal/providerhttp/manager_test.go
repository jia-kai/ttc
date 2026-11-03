package providerhttp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type idleTransport struct {
	roundTripFunc
	closed atomic.Int32
}

func (t *idleTransport) CloseIdleConnections() { t.closed.Add(1) }

func request(t *testing.T, ctx context.Context, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestClientPolicyAndUnsupportedResetStillOwnsClose(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	var calls atomic.Int32
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	redirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	original := &http.Client{Transport: base, Jar: jar, Timeout: time.Minute, CheckRedirect: redirect}
	client, manager := New(original)
	defer manager.Close()
	if client == original || client.Jar != jar || client.Timeout != original.Timeout || client.CheckRedirect == nil {
		t.Fatal("client policy changed")
	}
	if _, ok := original.Transport.(roundTripFunc); !ok {
		t.Fatal("original transport changed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.Do(request(t, ctx, "http://synthetic.test")); done <- err }()
	<-started
	if err := manager.Reset(); !errors.Is(err, ErrResetUnsupported) {
		t.Fatal("unsupported reset did not fail explicitly", err)
	}
	select {
	case err := <-done:
		t.Fatal("unsupported reset disrupted active request", err)
	default:
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrClosed) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
		t.Fatal("Close canceled parent or lost permanent failure", err, ctx.Err())
	}
	if _, err := client.Do(request(t, ctx, "http://synthetic.test")); !errors.Is(err, ErrClosed) || calls.Load() != 1 {
		t.Fatal("closed owner admitted future request", err, calls.Load())
	}
	if err := manager.Reset(); !errors.Is(err, ErrClosed) {
		t.Fatal("reset revived closed owner", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal("second close failed", err)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.active) != 0 {
		t.Fatal("closed exchanges retained", len(manager.active))
	}
}

func TestResetIndependentContextsAndUnderlyingFailures(t *testing.T) {
	started := make(chan struct{}, 2)
	ioFailure := errors.New("synthetic I/O failure")
	base := &idleTransport{roundTripFunc: func(req *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-req.Context().Done()
		return nil, ioFailure
	}}
	client, manager := New(&http.Client{Transport: base})
	defer manager.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, 2)
	for range 2 {
		go func() { _, err := client.Do(request(t, ctx, "http://synthetic.test")); results <- err }()
	}
	<-started
	<-started
	if err := manager.Reset(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-results; !errors.Is(err, syscall.ECONNABORTED) || !errors.Is(err, ioFailure) || errors.Is(err, context.Canceled) {
			t.Fatal("reset lost transport failure identity", err)
		}
	}
	if ctx.Err() != nil || base.closed.Load() != 1 {
		t.Fatal("reset changed parent context or missed idle cleanup", ctx.Err(), base.closed.Load())
	}
	// A canceled caller remains a caller cancellation, not a retryable reset.
	cancel()
	if _, err := client.Do(request(t, ctx, "http://synthetic.test")); !errors.Is(err, context.Canceled) || errors.Is(err, syscall.ECONNABORTED) {
		t.Fatal("caller cancellation changed classification", err)
	}
}

func TestCustomTLSDialersKeepPolicyAndIsolatePools(t *testing.T) {
	for _, contextual := range []bool{false, true} {
		t.Run(fmt.Sprintf("contextual=%t", contextual), func(t *testing.T) {
			base := &http.Transport{}
			if contextual {
				base.DialTLSContext = func(context.Context, string, string) (net.Conn, error) { t.Fatal("unexpected dial"); return nil, nil }
			} else {
				base.DialTLS = func(string, string) (net.Conn, error) { t.Fatal("unexpected dial"); return nil, nil }
			}
			original := &http.Client{Transport: base}
			client, manager := New(original)
			defer manager.Close()
			cloned := manager.base.(*http.Transport)
			if cloned == base || original.Transport != base || client.Transport != manager ||
				(cloned.DialTLSContext != nil) != contextual || (cloned.DialTLS != nil) == contextual {
				t.Fatal("custom TLS policy or caller pool ownership changed")
			}
			if err := manager.Reset(); !errors.Is(err, ErrResetUnsupported) {
				t.Fatal("custom TLS dialer advertised reset", err)
			}
		})
	}
}

func TestStandardProtocolPolicyPreserved(t *testing.T) {
	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	for _, test := range []struct {
		name       string
		base       *http.Transport
		forceHTTP2 bool
	}{
		{"automatic HTTP2", &http.Transport{}, true},
		{"explicit HTTP1", &http.Transport{Protocols: protocols}, false},
		{"custom TLS policy", &http.Transport{TLSClientConfig: &tls.Config{}}, false},
		{"custom ALPN policy", &http.Transport{TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}, false},
		{"custom raw dial policy", &http.Transport{DialContext: (&net.Dialer{}).DialContext}, false},
		{"explicit HTTP2", &http.Transport{ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, manager := New(&http.Client{Transport: test.base})
			defer manager.Close()
			if manager.template.ForceAttemptHTTP2 != test.forceHTTP2 {
				t.Fatal("raw dial wrapper changed automatic protocol selection")
			}
			if test.base.Protocols != nil && (!manager.template.Protocols.HTTP1() || manager.template.Protocols.HTTP2()) {
				t.Fatal("explicit protocol selection changed")
			}
			if (manager.template.TLSNextProto == nil) != (test.base.TLSNextProto == nil) {
				t.Fatal("explicit ALPN selection changed")
			}
		})
	}
}

func TestGenerationTracksOnlyOpenSocketsAndRejectsLateDial(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprintf("late=%t", late), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			conn, peer := net.Pipe()
			defer peer.Close()
			defer conn.Close()
			template := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
				close(entered)
				<-release
				return conn, nil
			}}
			transport, sockets := generation(template)
			done := make(chan error, 1)
			go func() {
				tracked, err := transport.DialContext(context.Background(), "tcp", "synthetic")
				if tracked != nil {
					err = tracked.Close()
				}
				done <- err
			}()
			<-entered
			if late {
				sockets.retire()
			}
			close(release)
			if err := <-done; (late && !errors.Is(err, syscall.ECONNABORTED)) || (!late && err != nil) {
				t.Fatal("dial retirement failed", err)
			}
			sockets.mu.Lock()
			defer sockets.mu.Unlock()
			if len(sockets.connections) != 0 {
				t.Fatal("closed connection retained", len(sockets.connections))
			}
		})
	}
}

func TestCustomTLSDialerPreservesHTTP2(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	original := server.Client()
	base := original.Transport.(*http.Transport)
	config := base.TLSClientConfig.Clone()
	config.NextProtos = []string{"h2", "http/1.1"}
	dialer := &tls.Dialer{Config: config}
	base.DialTLSContext = dialer.DialContext
	client, manager := New(original)
	defer manager.Close()
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 || resp.TLS == nil || resp.TLS.NegotiatedProtocol != "h2" {
		t.Fatal("custom TLS connection lost concrete TLS/ALPN behavior", resp.Proto, resp.TLS)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reset(); !errors.Is(err, ErrResetUnsupported) {
		t.Fatal("custom TLS reset claimed raw socket ownership", err)
	}
}

func TestResetAndCloseRetireStandardSockets(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		for _, closeOwner := range []bool{false, true} {
			for _, closeBody := range []bool{false, true} {
				t.Run(fmt.Sprintf("http2=%t/close=%t/bodyClosed=%t", http2, closeOwner, closeBody), func(t *testing.T) {
					closed := make(chan struct{}, 4)
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
					source.Transport.(*http.Transport).IdleConnTimeout = 0
					client, manager := New(source)
					defer manager.Close()
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					resp, err := client.Do(request(t, ctx, server.URL))
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					if http2 && resp.ProtoMajor != 2 {
						t.Fatal("TLS/ALPN did not negotiate HTTP/2")
					}
					if closeBody {
						resp.Body.Close()
					}
					manager.mu.Lock()
					retired := manager.sockets
					manager.mu.Unlock()
					want := error(syscall.ECONNABORTED)
					if closeOwner {
						err, want = manager.Close(), ErrClosed
					} else {
						err = manager.Reset()
					}
					if err != nil {
						t.Fatal(err)
					}
					if !closeBody {
						if _, err := io.ReadAll(resp.Body); !errors.Is(err, want) || errors.Is(err, context.Canceled) {
							t.Fatal("active body failure classification", err)
						}
					}
					select {
					case <-closed:
					case <-ctx.Done():
						t.Fatal("retired HTTP socket remained open", ctx.Err())
					}
					retired.mu.Lock()
					defer retired.mu.Unlock()
					if len(retired.connections) != 0 || !retired.retired {
						t.Fatal("retired generation retained raw sockets", len(retired.connections))
					}
					manager.mu.Lock()
					defer manager.mu.Unlock()
					if len(manager.active) != 0 || ctx.Err() != nil {
						t.Fatal("retired exchanges retained or caller canceled", len(manager.active), ctx.Err())
					}
				})
			}
		}
	}
}

func TestClosePreservesCallerStandardPool(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	base := &http.Transport{}
	defer base.CloseIdleConnections()
	original := &http.Client{Transport: base, Timeout: 5 * time.Second}
	client, manager := New(original)
	defer manager.Close()
	fetch := func(client *http.Client) {
		t.Helper()
		resp, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil || string(raw) != "ok" {
			t.Fatal(string(raw), err)
		}
	}
	fetch(original)
	fetch(client)
	if connections.Load() != 2 {
		t.Fatal("provider shared caller's standard pool", connections.Load())
	}
	manager.sockets.mu.Lock()
	open := len(manager.sockets.connections)
	manager.sockets.mu.Unlock()
	if open != 1 {
		t.Fatal("idle provider socket was not tracked", open)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	manager.sockets.mu.Lock()
	open = len(manager.sockets.connections)
	manager.sockets.mu.Unlock()
	if open != 0 {
		t.Fatal("Close retained an idle provider socket", open)
	}
	fetch(original)
	if connections.Load() != 2 {
		t.Fatal("Close retired caller-owned standard socket", connections.Load())
	}
}

func TestConcurrentResetCloseAndAdmission(t *testing.T) {
	var calls atomic.Int32
	base := &idleTransport{roundTripFunc: func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		default:
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
		}
	}}
	client, manager := New(&http.Client{Transport: base})
	defer manager.Close()
	var group sync.WaitGroup
	for range 20 {
		group.Go(func() {
			for range 10 {
				resp, _ := client.Get("http://synthetic.test")
				if resp != nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		})
	}
	group.Go(func() {
		for range 10 {
			manager.Reset()
		}
	})
	group.Go(func() { manager.Close() })
	group.Wait()
	before := calls.Load()
	if _, err := client.Get("http://synthetic.test"); !errors.Is(err, ErrClosed) || calls.Load() != before {
		t.Fatal("Close lost admission race", err, before, calls.Load())
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.active) != 0 {
		t.Fatal("concurrent shutdown retained exchanges", len(manager.active))
	}
}
