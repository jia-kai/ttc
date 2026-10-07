package openai

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func TestStreamDiagnosticsRedactAccessTokenBeforeTruncation(t *testing.T) {
	const token = "PRIVATE_ACCESS_<fixture>-abcdefghijklmnopqrstuvwxyz"
	fields := map[string]string{
		"type": "backend_" + token, "code": strings.Repeat("c", 240) + token,
		"message": strings.Repeat("m", 2030) + token + " retry", "param": "input_" + token,
	}
	for _, tc := range []struct {
		name   string
		status int
		event  any
	}{
		{"HTTP nested", 401, map[string]any{"error": fields}},
		{"SSE flat", 200, map[string]any{"type": "error", "code": fields["code"], "message": fields["message"], "param": fields["param"], "response_id": "resp_" + token}},
		{"SSE nested", 200, map[string]any{"type": "error", "error": fields}},
		{"SSE response", 200, map[string]any{"type": "response.failed", "response": map[string]any{"id": "resp_" + token, "error": fields}}},
		{"SSE incomplete", 200, map[string]any{"type": "response.incomplete", "response": map[string]any{"id": token, "incomplete_details": map[string]string{"reason": token}}}},
		// Exercise the terminal-envelope fallback after wireEvent decoding fails.
		{"SSE unexpected response type", 200, map[string]any{"type": "error", "response": map[string]any{"id": 123}, "error": fields}},
		{"SSE structural key", 200, map[string]any{"type": "error", token: true}},
	} {
		for _, recover := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recover=%t", tc.name, recover), func(t *testing.T) {
				body, err := json.Marshal(tc.event)
				if err != nil {
					t.Fatal(err)
				}
				// Decoded strings must be redacted, including escaped JSON tokens.
				body = []byte(strings.ReplaceAll(string(body), "PRIVATE_ACCESS_", `\u0050RIVATE_ACCESS_`))
				requests := 0
				a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
					requests++
					w.Header().Set("Retry-After", "0")
					w.Header().Set("x-request-id", strings.Repeat("r", 240)+token)
					if recover && requests == 2 {
						io.WriteString(w, sseFrames(map[string]any{"type": "response.completed"}))
						return
					}
					w.WriteHeader(tc.status)
					if tc.status == http.StatusOK {
						fmt.Fprintf(w, "data: %s\n\n", body)
					} else {
						w.Write(body)
					}
				})
				a.tokenSource = func(context.Context) (AccessTokens, error) {
					return AccessTokens{Access: token, AccountID: "account"}, nil
				}
				var notices []string
				err = a.Stream(context.Background(), partialRequest(0, 2), func(ev llm.StreamEvent) error {
					if ev.Kind == "retry" {
						notices = append(notices, ev.Retry.Reason)
					}
					return nil
				})
				if requests != 2 || len(notices) != 1 {
					t.Fatal("wrong attempt budget", requests, notices, err)
				}
				if recover {
					if err != nil {
						t.Fatal("did not recover", err)
					}
				} else {
					var transient *llm.TransientError
					if !errors.As(err, &transient) {
						t.Fatal("exhaustion lost classification", err)
					}
					notices = append(notices, err.Error())
				}
				for _, notice := range notices {
					assertSafeFailure(t, notice, "upstream attempt 2/2 failed: subscription stream terminated: ")
					if strings.Contains(notice, "PRIVATE_ACCESS") || strings.Contains(notice, token) || !strings.Contains(notice, "[redacted]") {
						t.Fatal("token or truncated token leaked", notice)
					}
				}
			})
		}
	}
}

func TestStreamPartialRecoveryNoticeRedactsAccessToken(t *testing.T) {
	const token = "PRIVATE_ACCESS_PARTIAL_FIXTURE"
	requests := 0
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("x-request-id", "request_"+token)
		io.WriteString(w, sseFrames(partialOutput("text"), map[string]any{
			"type": "response.failed", "response": map[string]any{
				"id": "response_" + token, "error": map[string]string{"message": "Invalid bearer " + token},
			},
		}))
	})
	a.tokenSource = func(context.Context) (AccessTokens, error) {
		return AccessTokens{Access: token, AccountID: "account"}, nil
	}
	err := a.Stream(context.Background(), partialRequest(0, 2), func(llm.StreamEvent) error { return nil })
	var partial *llm.PartialError
	if !errors.As(err, &partial) || requests != 1 {
		t.Fatal("committed failure did not hand off", err, requests)
	}
	for _, text := range []string{err.Error(), partial.Retry.Reason} {
		if strings.Contains(text, token) || !strings.Contains(text, "[redacted]") {
			t.Fatal("partial recovery notice leaked token", text)
		}
	}
}

func TestDiagnosticsRedactTokensLongerThanFieldLimit(t *testing.T) {
	token := "PRIVATE_ACCESS_" + strings.Repeat("secret", 400)
	raw, err := json.Marshal(map[string]any{"type": "error", "response_id": token, "error": map[string]string{"message": token}})
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []error{
		streamFailure(token, raw, "error"),
		httpFailure(token, &http.Response{StatusCode: 401, Header: http.Header{"X-Request-Id": []string{token}}, Body: io.NopCloser(strings.NewReader(string(raw)))}),
	} {
		assertFailureContains(t, failure, "response_id=[redacted]", "message=[redacted]")
		if strings.Contains(failure.Error(), "PRIVATE_ACCESS") || strings.Contains(failure.Error(), "secret") {
			t.Fatal("long token leaked a truncated prefix", failure)
		}
	}
}

func TestStreamOriginCancellationAndCertificatesAreFinal(t *testing.T) {
	private := "PRIVATE_ORIGIN_ERROR"
	for _, cause := range []struct {
		name string
		err  error
	}{
		{"canceled", context.Canceled},
		{"unknown authority", x509.UnknownAuthorityError{Cert: &x509.Certificate{}}},
		{"hostname", x509.HostnameError{Certificate: &x509.Certificate{}, Host: private}},
		{"invalid certificate", x509.CertificateInvalidError{Cert: &x509.Certificate{}, Reason: x509.Expired}},
	} {
		for _, origin := range []string{"transport", "HTTP 401 reader", "HTTP 500 reader", "SSE reader", "committed SSE reader", "no-tools SSE reader"} {
			t.Run(cause.name+"/"+origin, func(t *testing.T) {
				requests, retries, closes := 0, 0, 0
				a := adapterFixture(t, func(http.ResponseWriter, *http.Request) {})
				a.Client.Transport = retryTransport(func(*http.Request) (*http.Response, error) {
					requests++
					wrapped := fmt.Errorf("%s: %w", private, cause.err)
					if origin == "transport" {
						return nil, wrapped
					}
					status, prefix := http.StatusOK, ""
					switch origin {
					case "HTTP 401 reader":
						status = 401
					case "HTTP 500 reader":
						status = 500
					case "committed SSE reader", "no-tools SSE reader":
						prefix = sseFrames(partialOutput("text"))
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"0"}}, Body: &originFailureBody{
						Reader: io.MultiReader(strings.NewReader(prefix), &failureTestReader{err: wrapped}), closes: &closes,
					}}, nil
				})
				req := partialRequest(0, 3)
				req.NoTools = origin == "no-tools SSE reader"
				ctx := context.Background()
				err := a.Stream(ctx, req, func(ev llm.StreamEvent) error {
					if ev.Kind == "retry" {
						retries++
					}
					return nil
				})
				var transient *llm.TransientError
				var partial *llm.PartialError
				if err == nil || requests != 1 || retries != 0 || errors.As(err, &transient) || errors.As(err, &partial) || ctx.Err() != nil {
					t.Fatal("origin failure was retried or handed off", err, requests, retries)
				}
				if strings.Contains(err.Error(), private) {
					t.Fatal("arbitrary error text leaked", err)
				}
				if cause.name == "canceled" {
					if !errors.Is(err, context.Canceled) {
						t.Fatal("reader/transport cancellation lost identity", err)
					}
				} else if err.Error() != "subscription TLS certificate verification failed" {
					t.Fatal("certificate lost final diagnostic", err)
				}
				if origin != "transport" && closes != 1 {
					t.Fatal("response body not closed", closes)
				}
			})
		}
	}
}

func TestStreamRedirectPolicyFailureIsFinalAndValueFree(t *testing.T) {
	requests, policies, closes := 0, 0, 0
	a := adapterFixture(t, func(http.ResponseWriter, *http.Request) {})
	a.Client.Transport = retryTransport(func(req *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": []string{"https://redirect.test/PRIVATE_REDIRECT_PATH"}}, Request: req,
			Body: &originFailureBody{Reader: strings.NewReader("PRIVATE_REDIRECT_BODY"), closes: &closes}}, nil
	})
	a.Client.CheckRedirect = func(*http.Request, []*http.Request) error {
		policies++
		return errors.New("PRIVATE_REDIRECT_POLICY_ERROR")
	}
	err := a.Stream(context.Background(), partialRequest(0, 3), func(ev llm.StreamEvent) error {
		t.Error("local redirect failure emitted event", ev.Kind)
		return nil
	})
	var transient *llm.TransientError
	var partial *llm.PartialError
	if err == nil || err.Error() != "subscription redirect policy failed" || requests != 1 || policies != 1 || closes != 1 || errors.As(err, &transient) || errors.As(err, &partial) {
		t.Fatal("redirect policy retried, exposed details, or lost cleanup", err, requests, policies, closes)
	}
}

func TestStreamArbitraryTransportAndReaderFailuresRemainValueFree(t *testing.T) {
	for _, status := range []int{0, 200, 502} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			requests := 0
			a := adapterFixture(t, func(http.ResponseWriter, *http.Request) {})
			a.Client.Transport = retryTransport(func(*http.Request) (*http.Response, error) {
				requests++
				if status == 0 {
					return nil, errors.New("PRIVATE_TRANSPORT_ERROR")
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"0"}}, Body: io.NopCloser(&failureTestReader{err: errors.New("PRIVATE_READER_ERROR")})}, nil
			})
			var notices []string
			err := a.Stream(context.Background(), partialRequest(0, 2), func(ev llm.StreamEvent) error {
				if ev.Kind == "retry" {
					notices = append(notices, ev.Retry.Reason)
				}
				return nil
			})
			var transient *llm.TransientError
			if !errors.As(err, &transient) || requests != 2 || len(notices) != 1 {
				t.Fatal("arbitrary reader failure did not exhaust budget", err, requests, notices)
			}
			for _, text := range append(notices, err.Error()) {
				if strings.Contains(text, "PRIVATE_") {
					t.Fatal("reader error value leaked", text)
				}
			}
		})
	}
}

type originFailureBody struct {
	io.Reader
	closes *int
}

func (b *originFailureBody) Close() error {
	*b.closes++
	return nil
}
