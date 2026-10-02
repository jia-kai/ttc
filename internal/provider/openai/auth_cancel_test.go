package openai

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"scicode/internal/provider"
)

func TestAuthenticationWaitCanBeCanceledDuringAnotherRefresh(t *testing.T) {
	for _, operation := range []string{"stream", "catalog", "login"} {
		t.Run(operation, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-release
				io.WriteString(w, `{"access_token":"refreshed"}`)
			})
			a.credentials.Tokens.Access = "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`)) + ".signature"
			a.credentials.Tokens.Refresh = "synthetic-refresh"
			owner := make(chan error, 1)
			go func() { _, err := a.auth(context.Background()); owner <- err }()
			<-entered
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "stream":
					err = a.Stream(ctx, provider.Request{ConversationID: "test-conversation", Selection: provider.Selection{Provider: "openai", Model: provider.ScriptModel()}}, func(provider.StreamEvent) error { return nil })
				case "catalog":
					_, err = a.Models(ctx)
				case "login":
					err = a.Login(ctx, nil) // Cancellation must precede any UI or HTTP work.
				}
				done <- err
			}()
			var err error
			blocked := false
			select {
			case err = <-done:
			case <-time.After(time.Second):
				blocked = true
			}
			close(release)
			if ownerErr := <-owner; ownerErr != nil {
				t.Fatal("owner refresh failed", ownerErr)
			}
			if blocked {
				<-done
				t.Fatal("canceled request remained blocked behind another authentication operation")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("authentication wait lost cancellation", err)
			}
		})
	}
}

func TestCanceledOwnRefreshPreservesCancellation(t *testing.T) {
	for _, stage := range []string{"transport", "body"} {
		t.Run(stage, func(t *testing.T) {
			a := adapterFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected HTTP handler") })
			a.credentials.Tokens.Access = "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`)) + ".signature"
			a.credentials.Tokens.Refresh = "synthetic-refresh"
			a.Client.Transport = retryTransport(func(req *http.Request) (*http.Response, error) {
				if stage == "transport" {
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: canceledAuthBody{req.Context()}}, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			err := a.Stream(ctx, provider.Request{ConversationID: "test", Selection: provider.Selection{Provider: "openai", Model: provider.ScriptModel()}}, func(provider.StreamEvent) error { return nil })
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("own refresh lost cancellation: %T %v", err, err)
			}
		})
	}
}

type canceledAuthBody struct{ ctx context.Context }

func (b canceledAuthBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (canceledAuthBody) Close() error { return nil }

func TestRefreshFailuresPreserveRecoverability(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		transient bool
	}{
		{"transport", 0, true}, {"rate limit", 429, true},
		{"unavailable", 503, true}, {"expired refresh", 401, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := adapterFixture(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(test.status) })
			a.credentials.Tokens.Access = "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`)) + ".signature"
			a.credentials.Tokens.Refresh = "synthetic-refresh"
			if test.status == 0 {
				a.Client.Transport = retryTransport(func(*http.Request) (*http.Response, error) {
					return nil, errors.New("private transport diagnostic")
				})
			}
			err := a.Stream(context.Background(), provider.Request{ConversationID: "test", Selection: provider.Selection{Provider: "openai", Model: provider.ScriptModel()}, MaxAttempts: 1}, func(provider.StreamEvent) error { return nil })
			var transient *provider.TransientError
			if err == nil || errors.As(err, &transient) != test.transient || strings.Contains(err.Error(), "private") {
				t.Fatal("unsafe or unclassified refresh failure", err)
			}
		})
	}
}

func TestRefreshRetriesNotifyUIBeforeResponse(t *testing.T) {
	refreshes, responses, retries := 0, 0, 0
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/oauth/token") {
			refreshes++
			if refreshes == 1 {
				w.WriteHeader(503)
				return
			}
			io.WriteString(w, `{"access_token":"refreshed"}`)
			return
		}
		responses++
		io.WriteString(w, sseFrames(map[string]any{"type": "response.completed"}))
	})
	a.credentials.Tokens.Access = "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`)) + ".signature"
	a.credentials.Tokens.Refresh = "synthetic-refresh"
	err := a.Stream(context.Background(), provider.Request{ConversationID: "test", Selection: provider.Selection{Provider: "openai", Model: provider.ScriptModel()}, MaxAttempts: 2}, func(ev provider.StreamEvent) error {
		if ev.Kind == "retry" {
			retries++
		}
		return nil
	})
	if err != nil || refreshes != 2 || responses != 1 || retries != 1 {
		t.Fatal("refresh did not recover with UI notice", err, refreshes, responses, retries)
	}
}
