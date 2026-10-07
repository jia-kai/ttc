package openai

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ttc/internal/llm"
)

func TestRetryAfterAndSaturatedBackoff(t *testing.T) {
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"0", 0}, {"3", 3 * time.Second}, {"18446744073709551615", maxRetryDelay},
		{"18446744073709551615000000", maxRetryDelay},
		{now.Add(7 * time.Second).Format(http.TimeFormat), 7 * time.Second},
		{now.Add(-time.Hour).Format(http.TimeFormat), 0},
		{now.Add(time.Hour).Format(http.TimeFormat), maxRetryDelay},
	} {
		if got := retryDelay(100000, tc.header, now); got != tc.want {
			t.Errorf("%q: got %s, want %s", tc.header, got, tc.want)
		}
	}
	for _, attempt := range []int{0, 1, 4, 5, 100000} {
		for range 20 {
			got := retryDelay(attempt, "invalid", now)
			base := maxRetryDelay
			if attempt < 5 {
				base = time.Second * time.Duration(1<<attempt)
			}
			if got < time.Duration(float64(base)*0.75) || got > min(time.Duration(float64(base)*1.25), maxRetryDelay) {
				t.Fatalf("attempt %d: invalid jitter %s", attempt, got)
			}
		}
	}
}

func TestUnlimitedHTTPRetriesPreserveRequestAndRecover(t *testing.T) {
	var requests atomic.Int32
	var bodies []string
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		if r.Header.Get("session-id") != "test-conversation" {
			t.Error("cache identity changed during retry", r.Header.Get("session-id"))
		}
		if requests.Add(1) <= 4 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "secret backend detail")
			return
		}
		fmt.Fprint(w, sseFrames(map[string]any{"type": "response.output_text.delta", "delta": "Done"}, messageDone(0, "Done"), map[string]any{"type": "response.completed"}))
	})
	retries := []llm.Retry{}
	text := ""
	err := a.Stream(context.Background(), llm.Request{ConversationID: "test-conversation", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}}, func(ev llm.StreamEvent) error {
		if ev.Kind == "retry" {
			retries = append(retries, *ev.Retry)
		}
		text += ev.Text
		return nil
	})
	if err != nil || requests.Load() != 5 || len(retries) != 4 || text != "Done" {
		t.Fatal(err, requests.Load(), retries, text)
	}
	for i, retry := range retries {
		if retry != (llm.Retry{Attempt: i + 2, MaxAttempts: 0, Reason: "HTTP 503"}) {
			t.Fatal(retry)
		}
	}
	for _, body := range bodies {
		if body != bodies[0] {
			t.Fatal("request changed during retry")
		}
	}
}

func TestBoundedRetryAndPermanentHTTPFailures(t *testing.T) {
	for _, status := range []int{429, 503, 401, 400} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var count atomic.Int32
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(status)
			})
			attempts := []int{}
			err := a.Stream(context.Background(), llm.Request{ConversationID: "test-conversation", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}, MaxAttempts: 3}, func(ev llm.StreamEvent) error {
				attempts = append(attempts, ev.Retry.Attempt)
				if ev.Retry.MaxAttempts != 3 {
					t.Error(ev.Retry)
				}
				return nil
			})
			want := int32(1)
			if status == 429 || status == 503 {
				want = 3
			}
			if err == nil || count.Load() != want || len(attempts) != int(want)-1 {
				t.Fatal(err, count.Load(), attempts)
			}
		})
	}
}

func TestRetryNoticeFailureAndCancellationStopImmediately(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelWait), func(t *testing.T) {
			var count atomic.Int32
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) { count.Add(1); w.WriteHeader(429) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("notice persistence failed")
			err := a.Stream(ctx, llm.Request{ConversationID: "test-conversation", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}}, func(ev llm.StreamEvent) error {
				if ev.Kind != "retry" {
					t.Fatal(ev)
				}
				if cancelWait {
					cancel()
					return nil
				}
				return failure
			})
			want := failure
			if cancelWait {
				want = context.Canceled
			}
			if !errors.Is(err, want) || count.Load() != 1 {
				t.Fatal(err, count.Load())
			}
		})
	}
}

type retryTransport func(*http.Request) (*http.Response, error)

func (f retryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportAndEmptyStreamRetry(t *testing.T) {
	for _, transportFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(transportFailure), func(t *testing.T) {
			a := adapterFixture(t, func(http.ResponseWriter, *http.Request) {})
			count := 0
			a.Client = &http.Client{Transport: retryTransport(func(r *http.Request) (*http.Response, error) {
				count++
				if count == 1 && transportFailure {
					return nil, errors.New("secret transport details")
				}
				body := ""
				if count > 1 {
					body = "data: {\"type\":\"response.completed\"}\n\n"
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			kinds := []string{}
			err := a.Stream(context.Background(), llm.Request{ConversationID: "test-conversation", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}}, func(ev llm.StreamEvent) error {
				kinds = append(kinds, ev.Kind)
				if ev.Retry != nil && strings.Contains(ev.Retry.Reason, "secret") {
					t.Fatal(ev.Retry)
				}
				return nil
			})
			if err != nil || count != 2 || !reflect.DeepEqual(kinds, []string{"retry", "completed"}) {
				t.Fatal(err, count, kinds)
			}
		})
	}
}

func TestInvalidEndpointCertificateAndOversizedSSEDoNotRetry(t *testing.T) {
	for _, kind := range []string{"endpoint", "certificate", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			var count atomic.Int32
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				fmt.Fprint(w, "data: "+strings.Repeat("x", 8<<20)+"\n\n")
			})
			want := int32(1)
			if kind == "endpoint" {
				a.BaseURL = "ftp://invalid"
				want = 0
			}
			if kind == "certificate" {
				a.Client = &http.Client{Transport: retryTransport(func(*http.Request) (*http.Response, error) {
					count.Add(1)
					return nil, x509.UnknownAuthorityError{}
				})}
			}
			err := a.Stream(context.Background(), llm.Request{ConversationID: "test-conversation", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}}, func(ev llm.StreamEvent) error {
				t.Errorf("permanent failure emitted %s", ev.Kind)
				return errors.New("unexpected event")
			})
			if err == nil || count.Load() != want {
				t.Fatal(err, count.Load())
			}
		})
	}
}
