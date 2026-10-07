package openai

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const testCatalog = `{"models":[{"slug":"test","visibility":"list","context_window":100000}]}`

type catalogBody struct {
	io.Reader
	closed *int
}

func (b catalogBody) Close() error {
	*b.closed++
	return nil
}

func TestCatalogRetriesTransientHTTPAndClosesBodies(t *testing.T) {
	for _, status := range []int{429, 503} {
		for _, recover := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/recover=%t", status, recover), func(t *testing.T) {
				a := adapterFixture(t, nil)
				count, closed := 0, 0
				a.Client.Transport = retryTransport(func(r *http.Request) (*http.Response, error) {
					if closed != count {
						t.Fatal("retry did not close the previous response")
					}
					count++
					if r.Method != "GET" || r.URL.Path != "/models" || r.URL.Query().Get("client_version") != CatalogVersion || r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("ChatGPT-Account-ID") != "account" {
						t.Fatal("catalog retry changed request", r)
					}
					code := status
					if recover && count == 3 {
						code = 200
					}
					return &http.Response{StatusCode: code, Header: http.Header{"Retry-After": {"0"}}, Body: catalogBody{strings.NewReader(testCatalog), &closed}, Request: r}, nil
				})
				models, err := a.Models(context.Background())
				if count != 3 || closed != 3 {
					t.Fatal("catalog retries exceeded their limit or leaked bodies", count, closed)
				}
				if recover {
					if err != nil || len(models) != 1 || models[0].ID != "test" {
						t.Fatal(models, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d (attempt 3/3)", status)) {
					t.Fatal("missing final catalog failure", err)
				}
			})
		}
	}
}

func TestCatalogTransportRetriesAreBounded(t *testing.T) {
	for _, recover := range []bool{false, true} {
		t.Run(fmt.Sprintf("recover=%t", recover), func(t *testing.T) {
			t.Parallel()
			a := adapterFixture(t, nil)
			count := 0
			failure := errors.New("connection reset")
			a.Client.Transport = retryTransport(func(r *http.Request) (*http.Response, error) {
				count++
				if recover && count == 2 {
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(testCatalog)), Request: r}, nil
				}
				return nil, failure
			})
			models, err := a.Models(context.Background())
			if recover {
				if err != nil || len(models) != 1 || count != 2 {
					t.Fatal(models, count, err)
				}
			} else if !errors.Is(err, failure) || count != 3 || !strings.Contains(err.Error(), "attempt 3/3") {
				t.Fatal(count, err)
			}
		})
	}
}

func TestCatalogPermanentFailuresDoNotRetry(t *testing.T) {
	for _, kind := range []string{"401", "403", "404", "malformed", "certificate", "endpoint"} {
		t.Run(kind, func(t *testing.T) {
			count := 0
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				count++
				switch kind {
				case "401":
					w.WriteHeader(401)
				case "403":
					w.WriteHeader(403)
				case "404":
					w.WriteHeader(404)
				default:
					fmt.Fprint(w, "invalid JSON")
				}
			})
			want := 1
			if kind == "certificate" {
				a.Client.Transport = retryTransport(func(*http.Request) (*http.Response, error) {
					count++
					return nil, x509.UnknownAuthorityError{}
				})
			} else if kind == "endpoint" {
				a.BaseURL = "ftp://invalid"
				want = 0
			}
			if _, err := a.Models(context.Background()); err == nil || count != want {
				t.Fatal("permanent catalog failure retried", count, err)
			}
		})
	}
}

func TestCatalogCancellationAndDeadlineStopRetries(t *testing.T) {
	for _, phase := range []string{"before request", "backoff", "request"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if phase == "before request" {
				cancel()
			}
			count := 0
			a := adapterFixture(t, nil)
			a.Client.Transport = retryTransport(func(r *http.Request) (*http.Response, error) {
				count++
				if phase == "request" {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				return &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": {"30"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})
			want, wantCount := context.DeadlineExceeded, 1
			if phase == "before request" {
				want, wantCount = context.Canceled, 0
			}
			if _, err := a.Models(ctx); !errors.Is(err, want) || count != wantCount {
				t.Fatal("catalog ignored cancellation", count, err)
			}
		})
	}
}

func TestCatalogPreservesCallerDeadline(t *testing.T) {
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 30*time.Second {
			t.Fatal("catalog request has no bounded deadline", deadline, ok)
		}
		fmt.Fprint(w, testCatalog)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := a.Models(ctx); err != nil {
		t.Fatal(err)
	}
}
