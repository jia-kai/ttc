package openai

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"ttc/internal/llm"
)

func expiredUpstreamAuth(t *testing.T, handler http.HandlerFunc) *Authenticator {
	t.Helper()
	a := authenticatorFixture(t, handler)
	c := *a.credentials
	c.Tokens.Access = "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`)) + ".signature"
	c.Tokens.Refresh = "private-refresh-fixture"
	c.Tokens.ID = "private-id-fixture"
	if err := a.save(c); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAuthUpstreamRefreshFailuresShareStreamAttemptBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"bad request", 400, `{"error":"invalid_grant","error_description":"Refresh denied"}`, "code=invalid_grant"},
		{"unauthorized", 401, `{"error":{"type":"auth_error","code":"expired","message":"Refresh denied"}}`, "code=expired"},
		{"server error", 500, `{"error":{"message":"Try again"}}`, "message=Try again"},
		{"malformed success", 200, `{"access_token":"private-access-fixture", broken`, "invalid authentication response"},
		{"missing token", 200, `{"refresh_token":"private-response-refresh","id_token":"private-response-id"}`, "no access token"},
	} {
		for _, maxAttempts := range []int{1, 2, 0} {
			for _, recover := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/max=%d/recover=%t", tc.name, maxAttempts, recover), func(t *testing.T) {
					refreshes, responses := 0, 0
					a := expiredUpstreamAuth(t, func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != "/oauth/token" {
							responses++
							io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
							return
						}
						refreshes++
						w.Header().Set("Retry-After", "0")
						w.Header().Set("x-request-id", "refresh-request-fixture")
						if recover && refreshes == 2 {
							io.WriteString(w, `{"access_token":"refreshed"}`)
							return
						}
						w.WriteHeader(tc.status)
						io.WriteString(w, tc.body)
					})
					var retries []llm.Retry
					request := partialRequest(0, 0)
					request.MaxAttempts = maxAttempts
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					completions := 0
					err := transportAdapter(a).Stream(ctx, request, func(ev llm.StreamEvent) error {
						if ev.Kind == "retry" {
							retries = append(retries, *ev.Retry)
						}
						if ev.Kind == "completed" {
							completions++
						}
						return nil
					})
					limit := maxAttempts
					if limit == 0 {
						limit = llm.DefaultMaxAttempts
					}
					wantRefreshes, wantRetries, wantResponses := limit, limit-1, 0
					if recover && limit > 1 {
						wantRefreshes, wantRetries, wantResponses = 2, 1, 1
						if err != nil {
							t.Fatal("refresh did not recover", err)
						}
					} else {
						var transient *llm.TransientError
						if !errors.As(err, &transient) || !strings.HasPrefix(err.Error(), fmt.Sprintf("upstream attempt %d/%d failed: ", limit, limit)) {
							t.Fatal("refresh exhaustion lost upstream classification or attempt", err)
						}
						for _, want := range []string{tc.want, "request_id=refresh-request-fixture", "authentication"} {
							if !strings.Contains(err.Error(), want) {
								t.Errorf("final diagnostic lost %q: %v", want, err)
							}
						}
					}
					if refreshes != wantRefreshes || len(retries) != wantRetries || responses != wantResponses || completions != wantResponses {
						t.Fatal("incorrect refresh attempt budget", refreshes, retries, responses, completions)
					}
					for i, retry := range retries {
						if retry.Attempt != i+2 || retry.MaxAttempts != limit || retry.DelayMilliseconds != 0 || !strings.HasPrefix(retry.Reason, fmt.Sprintf("upstream attempt %d/%d failed: ", i+1, limit)) {
							t.Fatal("refresh retry metadata or Retry-After lost", retry)
						}
						for _, want := range []string{tc.want, "request_id=refresh-request-fixture"} {
							if !strings.Contains(retry.Reason, want) {
								t.Errorf("retry diagnostic lost %q: %s", want, retry.Reason)
							}
						}
						if strings.Contains(retry.Reason, "private-") {
							t.Fatal("refresh retry exposed tokens", retry)
						}
					}
				})
			}
		}
	}
}

func TestAuthUpstreamClassifiesAnyHTTPStatusAndPreservesRetryAfter(t *testing.T) {
	for _, status := range []int{300, 400, 401, 403, 404, 418, 429, 500, 599} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			a := expiredUpstreamAuth(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.Header().Set("x-request-id", "safe-refresh-id")
				w.WriteHeader(status)
				io.WriteString(w, `{"error":{"code":"refresh_denied","message":"Refresh failed"},"access_token":"private-access-fixture","refresh_token":"private-response-refresh","id_token":"private-response-id","arbitrary":"private-body-fixture"}`)
			})
			_, err := a.AccessTokens(context.Background())
			var transient *llm.TransientError
			if !errors.As(err, &transient) || credentialRetryAfter(fmt.Errorf("outer: %w", err)) != "7" {
				t.Fatal("refresh origin or Retry-After lost", err)
			}
			for _, want := range []string{fmt.Sprintf("HTTP %d", status), "code=refresh_denied", "message=Refresh failed", "request_id=safe-refresh-id"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refresh error lost %q: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), "private-") {
				t.Fatal("auth error exposed private response body", err)
			}
		})
	}
	if credentialRetryAfter(errors.New("local failure")) != "" {
		t.Fatal("local error had upstream Retry-After")
	}
}

func TestAuthUpstreamDiagnosticsBoundAndRedactAllowedFields(t *testing.T) {
	a := expiredUpstreamAuth(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-request-id", "refresh\n\x1bid"+strings.Repeat("x", 500))
		w.WriteHeader(401)
		message, err := json.Marshal("private-refresh-fixture private-id-fixture private-response-access private-response-refresh private-response-id\n\x1b[31m" + strings.Repeat("x", 3000))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(w, `{"error":{"code":"invalid_grant","message":%s},"access_token":"private-response-access","refresh_token":"private-response-refresh","id_token":"private-response-id","unknown":"private-arbitrary-body"}`, message)
	})
	_, err := a.AccessTokens(context.Background())
	if err == nil || len(err.Error()) > failureDiagnosticLimit || strings.ContainsAny(err.Error(), "\n\r\x1b") || strings.Contains(err.Error(), "private-") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatal("unsafe or unbounded auth diagnostic", err)
	}
}

func TestAuthUpstreamTransportAndBodyFailuresAreTransient(t *testing.T) {
	for _, stage := range []string{"transport", "body", "trailing JSON", "wrong token type", "oversized"} {
		t.Run(stage, func(t *testing.T) {
			a := expiredUpstreamAuth(t, func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected handler") })
			a.Client.Transport = retryTransport(func(*http.Request) (*http.Response, error) {
				if stage == "transport" {
					return nil, errors.New("private-transport-details")
				}
				var body io.ReadCloser = io.NopCloser(strings.NewReader(`{"access_token":"new"} {}`))
				switch stage {
				case "body":
					body = authUpstreamErrorBody{errors.New("private-body-details")}
				case "wrong token type":
					body = io.NopCloser(strings.NewReader(`{"access_token":123}`))
				case "oversized":
					body = io.NopCloser(strings.NewReader(strings.Repeat(" ", maxCredentialBytes+1)))
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"X-Request-Id": {"body-failure-id"}}, Body: body}, nil
			})
			_, err := a.AccessTokens(context.Background())
			var transient *llm.TransientError
			if !errors.As(err, &transient) || strings.Contains(err.Error(), "private-") {
				t.Fatal("unsafe or unclassified upstream failure", err)
			}
			if stage != "transport" && !strings.Contains(err.Error(), "request_id=body-failure-id") {
				t.Fatal("body failure lost upstream request ID", err)
			}
		})
	}
}

type authUpstreamErrorBody struct{ err error }

func (b authUpstreamErrorBody) Read([]byte) (int, error) { return 0, b.err }
func (authUpstreamErrorBody) Close() error               { return nil }

func TestAuthUpstreamLocalFailuresNeverRetry(t *testing.T) {
	for _, kind := range []string{"file", "lock", "save", "configuration", "header", "redirect policy", "TLS", "canceled", "cancel during refresh"} {
		t.Run(kind, func(t *testing.T) {
			refreshes := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a := expiredUpstreamAuth(t, func(w http.ResponseWriter, _ *http.Request) {
				refreshes++
				if kind == "redirect policy" {
					w.Header().Set("Location", "https://mock.test/elsewhere")
					w.WriteHeader(302)
					return
				}
				if kind == "cancel during refresh" {
					cancel()
				}
				io.WriteString(w, `{"access_token":"refreshed"}`)
			})
			// Change the save fixture after construction so the callback can capture a.
			if kind == "save" {
				a.Client.Transport = retryTransport(func(*http.Request) (*http.Response, error) {
					refreshes++
					if err := os.Remove(a.CredentialPath); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(a.CredentialPath, 0700); err != nil {
						t.Fatal(err)
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"access_token":"refreshed"}`))}, nil
				})
			}
			switch kind {
			case "file":
				if err := os.Remove(a.CredentialPath); err != nil {
					t.Fatal(err)
				}
			case "lock":
				if err := os.Mkdir(a.CredentialPath+".lock", 0700); err != nil {
					t.Fatal(err)
				}
			case "configuration":
				a.AuthURL = "ftp://mock.test"
			case "header":
				a.AuthURL = "http://bad host"
			case "redirect policy":
				a.Client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("private-policy-details") }
			case "TLS":
				a.Client.Transport = retryTransport(func(*http.Request) (*http.Response, error) {
					refreshes++
					return nil, x509.UnknownAuthorityError{}
				})
			case "canceled":
				cancel()
			}
			err := transportAdapter(a).Stream(ctx, partialRequest(0, 0), func(ev llm.StreamEvent) error {
				t.Error("local auth failure emitted event", ev.Kind)
				return nil
			})
			var transient *llm.TransientError
			if err == nil || errors.As(err, &transient) || refreshes > 1 {
				t.Fatal("local auth failure retried", err, refreshes)
			}
			if strings.Contains(kind, "cancel") && !errors.Is(err, context.Canceled) {
				t.Fatal("lost cancellation", err)
			}
		})
	}
}

func TestAuthUpstreamLoginDoesNotGainRefreshRetryPolicy(t *testing.T) {
	for _, status := range []int{400, 401, 500, 200} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			requests := 0
			a := authenticatorFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				requests++
				w.WriteHeader(status)
				io.WriteString(w, "malformed login response containing private-body")
			})
			err := a.Login(context.Background(), &testLogin{})
			var transient *llm.TransientError
			if err == nil || errors.As(err, &transient) || requests != 1 || strings.Contains(err.Error(), "private-body") {
				t.Fatal("Login incorrectly inherited refresh retry policy", err, requests)
			}
		})
	}
}
