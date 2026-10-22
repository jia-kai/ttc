package openai

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type idleTestTransport func(*http.Request) (*http.Response, error)

func (f idleTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type idleTestBody struct {
	read  func([]byte) (int, error)
	close func() error
}

func (b idleTestBody) Read(p []byte) (int, error) { return b.read(p) }
func (b idleTestBody) Close() error {
	if b.close != nil {
		return b.close()
	}
	return nil
}

func idleTestRequest(t *testing.T, ctx context.Context) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://idle.test/response", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func idleTestResponse(body io.ReadCloser) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}
}

func idleTestTimeout(t *testing.T, err error) {
	t.Helper()
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("wanted net.Error timeout, got %T: %v", err, err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("idle timeout was classified as caller cancellation: %v", err)
	}
}

func idleTestCanceled(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("derived request context was not canceled promptly")
	}
}

func idleTestDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestDoIdleHTTPNoHeadersTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var derived context.Context
	client := &http.Client{Transport: idleTestTransport(func(req *http.Request) (*http.Response, error) {
		derived = req.Context()
		deadline, ok := derived.Deadline()
		parentDeadline, _ := ctx.Deadline()
		if !ok || !deadline.Equal(parentDeadline) {
			t.Error("inactivity helper replaced the caller deadline with an overall deadline")
		}
		<-derived.Done()
		return nil, derived.Err()
	})}
	_, err := doIdleHTTP(client, idleTestRequest(t, ctx), 100*time.Millisecond)
	idleTestTimeout(t, err)
	idleTestCanceled(t, derived)
	if ctx.Err() != nil {
		t.Fatalf("idle expiry canceled the caller context: %v", ctx.Err())
	}
}

func TestDoIdleHTTPBodyStallTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var derived context.Context
	client := &http.Client{Transport: idleTestTransport(func(req *http.Request) (*http.Response, error) {
		derived = req.Context()
		return idleTestResponse(idleTestBody{read: func([]byte) (int, error) {
			<-derived.Done()
			return 0, derived.Err()
		}}), nil
	})}
	resp, err := doIdleHTTP(client, idleTestRequest(t, ctx), 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, err = resp.Body.Read(make([]byte, 1))
	idleTestTimeout(t, err)
	idleTestCanceled(t, derived)
}

func TestDoIdleHTTPRawActivityHasNoOverallDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	const idle = 240 * time.Millisecond
	// No complete SSE event is ever emitted: comments and partial frames still
	// count as transport activity, independently of the stream parser.
	chunks := []string{":", " heartbeat", "\n", ":", " keepalive\n", "data:", " {", "\"partial\":"}
	var derived context.Context
	client := &http.Client{Transport: idleTestTransport(func(req *http.Request) (*http.Response, error) {
		derived = req.Context()
		index := 0
		return idleTestResponse(idleTestBody{read: func(p []byte) (int, error) {
			if index == len(chunks) {
				return 0, io.EOF
			}
			if err := idleTestDelay(derived, 60*time.Millisecond); err != nil {
				return 0, err
			}
			n := copy(p, chunks[index])
			index++
			return n, nil
		}}), nil
	})}
	start := time.Now()
	resp, err := doIdleHTTP(client, idleTestRequest(t, ctx), idle)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil || string(got) != strings.Join(chunks, "") {
		t.Fatalf("raw activity did not keep exchange alive: %q, %v", got, err)
	}
	if time.Since(start) <= idle {
		t.Fatal("fixture did not outlast the inactivity interval")
	}
	idleTestCanceled(t, derived)
}

func TestDoIdleHTTPHeadersAndFirstByteResetTimer(t *testing.T) {
	for _, stage := range []string{"headers", "first byte"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			client := &http.Client{Transport: idleTestTransport(func(req *http.Request) (*http.Response, error) {
				if err := idleTestDelay(req.Context(), 160*time.Millisecond); err != nil {
					return nil, err
				}
				if stage == "first byte" {
					trace := httptrace.ContextClientTrace(req.Context())
					if trace == nil || trace.GotFirstResponseByte == nil {
						return nil, errors.New("missing first-response-byte trace")
					}
					trace.GotFirstResponseByte()
					if err := idleTestDelay(req.Context(), 160*time.Millisecond); err != nil {
						return nil, err
					}
				}
				read := false
				return idleTestResponse(idleTestBody{read: func(p []byte) (int, error) {
					if read {
						return 0, io.EOF
					}
					if err := idleTestDelay(req.Context(), 160*time.Millisecond); err != nil {
						return 0, err
					}
					read = true
					return copy(p, "x"), nil
				}}), nil
			})}
			resp, err := doIdleHTTP(client, idleTestRequest(t, ctx), 260*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil || string(got) != "x" {
				t.Fatalf("%s did not reset inactivity timer: %q, %v", stage, got, err)
			}
		})
	}
}

func TestDoIdleHTTPCleanup(t *testing.T) {
	for _, terminal := range []string{"close", "EOF", "read error", "transport error"} {
		t.Run(terminal, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var derived context.Context
			var closed atomic.Bool
			failure := errors.New("synthetic failure")
			client := &http.Client{Transport: idleTestTransport(func(req *http.Request) (*http.Response, error) {
				derived = req.Context()
				if _, ok := derived.Deadline(); ok {
					t.Error("helper added an overall request deadline")
				}
				if terminal == "transport error" {
					return nil, failure
				}
				return idleTestResponse(idleTestBody{
					read: func([]byte) (int, error) {
						if terminal == "read error" {
							return 0, failure
						}
						return 0, io.EOF
					},
					close: func() error { closed.Store(true); return nil },
				}), nil
			})}
			resp, err := doIdleHTTP(client, idleTestRequest(t, ctx), 5*time.Second)
			if terminal == "transport error" {
				if !errors.Is(err, failure) {
					t.Fatalf("lost transport error: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if terminal == "close" {
					if err := resp.Body.Close(); err != nil {
						t.Fatal(err)
					}
					if !closed.Load() {
						t.Error("Close did not close the underlying response body")
					}
				} else {
					_, err := resp.Body.Read(make([]byte, 1))
					want := error(io.EOF)
					if terminal == "read error" {
						want = failure
					}
					if !errors.Is(err, want) {
						t.Fatalf("lost terminal body error: %v", err)
					}
				}
			}
			idleTestCanceled(t, derived)
			if ctx.Err() != nil {
				t.Fatal("cleanup canceled the caller context")
			}
		})
	}
}

func TestDoIdleHTTPParentCancellation(t *testing.T) {
	for _, stage := range []string{"headers", "body"} {
		for _, deadline := range []bool{false, true} {
			name := stage + "/cancel"
			if deadline {
				name = stage + "/deadline"
			}
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				if deadline {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
				}
				defer cancel()
				var derived context.Context
				client := &http.Client{Transport: idleTestTransport(func(req *http.Request) (*http.Response, error) {
					derived = req.Context()
					if stage == "headers" {
						if !deadline {
							cancel()
						}
						<-derived.Done()
						return nil, derived.Err()
					}
					return idleTestResponse(idleTestBody{read: func([]byte) (int, error) {
						if !deadline {
							cancel()
						}
						<-derived.Done()
						return 0, derived.Err()
					}}), nil
				})}
				resp, err := doIdleHTTP(client, idleTestRequest(t, ctx), 5*time.Second)
				if stage == "body" {
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					_, err = resp.Body.Read(make([]byte, 1))
				}
				if !errors.Is(err, ctx.Err()) || ctx.Err() == nil {
					t.Fatalf("lost parent cancellation: parent=%v, result=%v", ctx.Err(), err)
				}
				idleTestCanceled(t, derived)
			})
		}
	}
}

func TestDoIdleHTTPIndependentRequestsAndClientConfiguration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var firstBytes atomic.Int32
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotFirstResponseByte: func() { firstBytes.Add(1) }})
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	redirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	transport := idleTestTransport(func(req *http.Request) (*http.Response, error) {
		httptrace.ContextClientTrace(req.Context()).GotFirstResponseByte()
		if req.URL.Path == "/stalled" {
			return idleTestResponse(idleTestBody{read: func([]byte) (int, error) {
				<-req.Context().Done()
				return 0, req.Context().Err()
			}}), nil
		}
		remaining := 8
		return idleTestResponse(idleTestBody{read: func(p []byte) (int, error) {
			if remaining == 0 {
				return 0, io.EOF
			}
			if err := idleTestDelay(req.Context(), 60*time.Millisecond); err != nil {
				return 0, err
			}
			remaining--
			return copy(p, ":"), nil
		}}), nil
	})
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, Jar: jar, CheckRedirect: redirect}
	var wg sync.WaitGroup
	for _, path := range []string{"/stalled", "/active"} {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			req := idleTestRequest(t, ctx)
			req.URL.Path = path
			resp, err := doIdleHTTP(client, req, 240*time.Millisecond)
			if err != nil {
				t.Errorf("%s headers: %v", path, err)
				return
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if path == "/stalled" {
				idleTestTimeout(t, err)
			} else if err != nil || string(data) != "::::::::" {
				t.Errorf("independent active request failed: %q, %v", data, err)
			}
		}(path)
	}
	wg.Wait()
	if firstBytes.Load() != 2 {
		t.Fatalf("caller trace was lost: %d first-byte calls", firstBytes.Load())
	}
	if client.Timeout != 2*time.Second || client.Jar != jar || client.CheckRedirect == nil {
		t.Fatal("helper modified the caller's client configuration")
	}
	if _, ok := client.Transport.(idleTestTransport); !ok {
		t.Fatal("helper replaced the caller's transport")
	}
}

// Closing client connections before Server.Close ensures even a failing test
// unblocks handlers waiting for network cancellation.
func idleTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer close(done)
		handler(w, req)
	}))
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	return server, done
}

func idleTestHandlerDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("HTTP handler did not finish after request cancellation/completion")
	}
}

func TestDoIdleHTTPLocalSSEActivityThenStall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const idle = 450 * time.Millisecond
	chunks := []string{":", " heartbeat", "\n", ":", " keepalive\n", "data:", " {", "\"partial\":"}
	server, handlerDone := idleTestServer(t, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for _, chunk := range chunks {
			if idleTestDelay(req.Context(), 100*time.Millisecond) != nil {
				return
			}
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
		<-req.Context().Done()
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := doIdleHTTP(server.Client(), req, idle)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	idleTestTimeout(t, err)
	if string(data) != strings.Join(chunks, "") {
		t.Fatalf("activity was interrupted before the deliberate stall: %q", data)
	}
	if time.Since(start) <= 2*idle {
		t.Fatal("fixture did not remain active beyond the initial idle interval")
	}
	if ctx.Err() != nil {
		t.Fatalf("parent deadline, not inactivity, ended the stream: %v", ctx.Err())
	}
	// Wait before Close: timeout itself must cancel the network request.
	idleTestHandlerDone(t, handlerDone)
}

func TestDoIdleHTTPLocalClientTimeout(t *testing.T) {
	for _, stage := range []string{"headers", "body"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server, handlerDone := idleTestServer(t, func(w http.ResponseWriter, req *http.Request) {
				if stage == "body" {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				<-req.Context().Done()
			})
			client := server.Client()
			client.Timeout = 150 * time.Millisecond
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			resp, err := doIdleHTTP(client, req, 2*time.Second)
			if stage == "body" {
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				_, err = io.ReadAll(resp.Body)
			}
			idleTestTimeout(t, err)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("client's hard timeout was replaced by idle expiry: %v", err)
			}
			if time.Since(start) >= 2*time.Second {
				t.Fatal("client timeout did not end the exchange before the longer idle interval")
			}
			if ctx.Err() != nil || client.Timeout != 150*time.Millisecond {
				t.Fatalf("caller state changed: context=%v, client timeout=%v", ctx.Err(), client.Timeout)
			}
			idleTestHandlerDone(t, handlerDone)
		})
	}
}

func TestDoIdleHTTPLocalInformationalHeaders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const idle = 450 * time.Millisecond
	const hints = 8
	firstByte := make(chan time.Time, 1)
	var receivedHints atomic.Int32
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotFirstResponseByte: func() { firstByte <- time.Now() },
		Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
			if code != http.StatusEarlyHints || header.Get("Link") != "</style.css>; rel=preload" {
				t.Errorf("unexpected informational response: %d, %v", code, header)
			}
			receivedHints.Add(1)
			return nil
		},
	})
	server, handlerDone := idleTestServer(t, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Link", "</style.css>; rel=preload")
		for range hints {
			if idleTestDelay(req.Context(), 100*time.Millisecond) != nil {
				return
			}
			// net/http flushes 1xx headers immediately. Flusher.Flush here
			// would instead finalize the response with an implicit 200.
			w.WriteHeader(http.StatusEarlyHints)
		}
		if idleTestDelay(req.Context(), 100*time.Millisecond) != nil {
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := doIdleHTTP(server.Client(), req, idle)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || string(data) != "ok" {
		t.Fatalf("informational activity failed to keep headers alive: status=%d, body=%q, error=%v", resp.StatusCode, data, err)
	}
	if receivedHints.Load() != hints {
		t.Fatalf("caller informational trace lost: got %d hints, want %d", receivedHints.Load(), hints)
	}
	select {
	case first := <-firstByte:
		if time.Since(first) <= idle {
			t.Fatal("header wait did not outlast the first-byte inactivity interval")
		}
	default:
		t.Fatal("caller first-byte trace was lost")
	}
	idleTestHandlerDone(t, handlerDone)
}
