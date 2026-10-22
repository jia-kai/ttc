package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ttc/internal/llm"
)

type stalledIdleBody struct {
	ctx    context.Context
	prefix *strings.Reader
}

func (b *stalledIdleBody) Read(p []byte) (int, error) {
	if b.prefix.Len() != 0 {
		return b.prefix.Read(p)
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (*stalledIdleBody) Close() error { return nil }

func idleStreamAdapter(transport http.RoundTripper) *Adapter {
	a := NewAdapter(Config{Client: &http.Client{Transport: transport}, BaseURL: "https://mock.test",
		TokenSource: func(ctx context.Context) (AccessTokens, error) {
			return AccessTokens{Access: "synthetic-token", AccountID: "account"}, ctx.Err()
		}})
	a.idleTimeout = 75 * time.Millisecond
	return a
}

func TestResponseIdleBeforeOutputUsesRetryBudget(t *testing.T) {
	for _, phase := range []string{"headers", "body", "heartbeat only"} {
		t.Run(phase, func(t *testing.T) {
			var requests atomic.Int32
			a := idleStreamAdapter(retryTransport(func(req *http.Request) (*http.Response, error) {
				if requests.Add(1) == 1 {
					if phase == "headers" {
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					prefix := ""
					if phase == "heartbeat only" {
						prefix = ": heartbeat\n\n"
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Retry-After": []string{"0"}}, Body: &stalledIdleBody{ctx: req.Context(), prefix: strings.NewReader(prefix)}}, nil
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sseFrames(
					map[string]any{"type": "response.output_text.delta", "delta": "Done"}, messageDone(0, "Done"), map[string]any{"type": "response.completed"})))}, nil
			}))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			retries, text := 0, ""
			err := a.Stream(ctx, partialRequest(0, 2), func(event llm.StreamEvent) error {
				if event.Kind == "retry" {
					retries++
					if !strings.Contains(event.Retry.Reason, "model response inactive for 75ms") {
						t.Error("idle failure lost diagnostic", event.Retry.Reason)
					}
				}
				text += event.Text
				return nil
			})
			if err != nil || requests.Load() != 2 || retries != 1 || text != "Done" || ctx.Err() != nil {
				t.Fatal("idle failure did not recover", err, requests.Load(), retries, text)
			}
		})
	}
}

func TestResponseIdleAfterOutputHandsOffWithoutReplay(t *testing.T) {
	var requests atomic.Int32
	a := idleStreamAdapter(retryTransport(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &stalledIdleBody{ctx: req.Context(), prefix: strings.NewReader(sseFrames(partialOutput("text")))}}, nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	text := ""
	err := a.Stream(ctx, partialRequest(0, 2), func(event llm.StreamEvent) error { text += event.Text; return nil })
	var partial *llm.PartialError
	var idle *idleHTTPError
	if !errors.As(err, &partial) || !errors.As(err, &idle) || errors.Is(err, context.Canceled) || requests.Load() != 1 || text != "partial" || ctx.Err() != nil {
		t.Fatal("idle timeout lost partial recovery semantics", err, requests.Load(), text)
	}
}

func TestResponseIdleDiscardsBufferedNoToolsPartialBeforeRetry(t *testing.T) {
	var requests atomic.Int32
	a := idleStreamAdapter(retryTransport(func(req *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			return &http.Response{StatusCode: 200, Header: http.Header{"Retry-After": []string{"0"}}, Body: &stalledIdleBody{ctx: req.Context(), prefix: strings.NewReader(sseFrames(partialOutput("text")))}}, nil
		}
		return idleTestResponse(io.NopCloser(strings.NewReader(sseFrames(map[string]any{"type": "response.output_text.delta", "delta": "complete"}, messageDone(0, "complete"), map[string]any{"type": "response.completed"})))), nil
	}))
	req := partialRequest(0, 2)
	req.NoTools = true
	var text strings.Builder
	err := a.Stream(context.Background(), req, func(event llm.StreamEvent) error { text.WriteString(event.Text); return nil })
	if err != nil || requests.Load() != 2 || text.String() != "complete" {
		t.Fatal("buffered partial escaped or prevented retry", err, requests.Load(), text.String())
	}
}

func TestResponseIdleBoundsCatalogAndAuthBodies(t *testing.T) {
	transport := retryTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Retry-After": []string{"0"}}, Body: &stalledIdleBody{ctx: req.Context(), prefix: strings.NewReader("")}}, nil
	})
	a := idleStreamAdapter(transport)
	if _, err := a.Models(context.Background()); err == nil || !strings.Contains(err.Error(), "inactive") || errors.Is(err, context.Canceled) {
		t.Fatal("catalog body stall not bounded", err)
	}
	auth := NewAuthenticator(filepath.Join(t.TempDir(), "not-read.json"))
	auth.Client = &http.Client{Transport: transport}
	auth.idleTimeout = a.idleTimeout
	var out map[string]any
	if _, err := auth.json(context.Background(), "https://mock.test/token", map[string]string{"synthetic": "input"}, &out); err == nil || !strings.Contains(err.Error(), "inactive") || errors.Is(err, context.Canceled) {
		t.Fatal("auth body stall not bounded", err)
	}
}

func TestCatalogResponseBodyIdleRetriesAndRecovers(t *testing.T) {
	var requests atomic.Int32
	a := idleStreamAdapter(retryTransport(func(req *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			return &http.Response{StatusCode: 200, Header: http.Header{"Retry-After": []string{"0"}}, Body: &stalledIdleBody{ctx: req.Context(), prefix: strings.NewReader("")}}, nil
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"models":[{"slug":"tiny","visibility":"list","context_window":100}]}`))}, nil
	}))
	models, err := a.Models(context.Background())
	if err != nil || requests.Load() != 2 || len(models) != 1 || models[0].ID != "tiny" {
		t.Fatal("catalog body timeout did not recover", models, requests.Load(), err)
	}
}

func TestAuthIdleErrorRedactsNewResponseTokens(t *testing.T) {
	const token = "SYNTHETIC_NEW_ACCESS_TOKEN"
	for _, tc := range []struct {
		name     string
		status   int
		complete bool
	}{
		{"success complete", 200, true}, {"success incomplete", 200, false},
		{"error complete", 500, true}, {"error incomplete", 500, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAuthenticator(filepath.Join(t.TempDir(), "not-read.json"))
			a.idleTimeout = 75 * time.Millisecond
			a.Client = &http.Client{Transport: retryTransport(func(req *http.Request) (*http.Response, error) {
				body := `{"access_token":"` + token + `"}`
				if !tc.complete {
					body = strings.TrimSuffix(body, "}")
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"X-Request-Id": []string{token}},
					Body: &stalledIdleBody{ctx: req.Context(), prefix: strings.NewReader(body)}}, nil
			})}
			var out map[string]any
			_, err := a.json(context.Background(), "https://mock.test/token", nil, &out)
			if err == nil || strings.Contains(err.Error(), token) || (tc.status == 200 && !strings.Contains(err.Error(), "inactive")) {
				t.Fatal("idle diagnostic exposed a newly received token", err)
			}
		})
	}
}

func TestLoginExchangePreservesIdleErrors(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			a := NewAuthenticator(filepath.Join(t.TempDir(), "not-written.json"))
			a.AuthURL = "https://mock.test"
			a.idleTimeout = 75 * time.Millisecond
			a.Client = &http.Client{Transport: retryTransport(func(req *http.Request) (*http.Response, error) {
				var body string
				switch req.URL.Path {
				case "/api/accounts/deviceauth/usercode":
					body = `{"device_auth_id":"d","user_code":"CODE","interval":"1"}`
				case "/api/accounts/deviceauth/token":
					body = `{"authorization_code":"authorized","code_verifier":"verify"}`
				case "/oauth/token":
					if phase == "headers" {
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &stalledIdleBody{ctx: req.Context(), prefix: strings.NewReader("")}}, nil
				default:
					t.Error("unexpected auth path", req.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := a.Login(ctx, &testLogin{})
			var idle *idleHTTPError
			if !errors.As(err, &idle) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
				t.Fatal("token exchange lost idle timeout category", err)
			}
		})
	}
}
