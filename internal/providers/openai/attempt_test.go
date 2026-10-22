package openai

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ttc/internal/llm"
)

type fakeRemoteProvider struct {
	cancel func(context.Context, string) error
}

func (p fakeRemoteProvider) Cancel(ctx context.Context, responseID string) error {
	return p.cancel(ctx, responseID)
}

type trackedAttemptBody struct {
	*stalledIdleBody
	closed atomic.Bool
}

func (b *trackedAttemptBody) Close() error { b.closed.Store(true); return nil }

// Exercise the same ownership boundary as Stream with a backend that really
// implements remote cancellation, rather than Codex's explicit no-op.
func TestAttemptCancellationLifecycle(t *testing.T) {
	consumerErr := errors.New("consumer stopped")
	for _, scenario := range []string{"initial silence", "body timeout", "user", "consumer", "malformed", "eof", "success"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var x *responseExchange
			var body *trackedAttemptBody
			var calls atomic.Int32
			provider := fakeRemoteProvider{cancel: func(cleanup context.Context, id string) error {
				calls.Add(1)
				if cleanup.Err() != nil {
					t.Error("cleanup inherited canceled context")
				}
				deadline, ok := cleanup.Deadline()
				if !ok || time.Until(deadline) > remoteCancelTimeout {
					t.Error("cleanup is not bounded")
				}
				if x.ctx.Err() == nil {
					t.Error("remote cancel preceded local abort")
				}
				if body != nil && !body.closed.Load() {
					t.Error("body not closed before remote cancel")
				}
				wantID := "remote-this-attempt"
				if scenario == "initial silence" || scenario == "eof" {
					wantID = ""
				}
				if id != wantID {
					t.Errorf("wrong remote attempt: %q, want %q", id, wantID)
				}
				return errors.New("best-effort cancellation failure")
			}}
			x = newResponseExchange(ctx, provider.Cancel, 75*time.Millisecond)
			prefix := sseFrames(map[string]any{"type": "response.created", "response": map[string]any{"id": "remote-this-attempt"}})
			switch scenario {
			case "initial silence", "eof":
				prefix = ""
			case "user", "consumer":
				prefix += sseFrames(partialOutput("text"))
			case "malformed":
				prefix += "data: {broken\n\n"
			case "success":
				prefix += sseFrames(map[string]any{"type": "response.completed"})
			}
			body = &trackedAttemptBody{stalledIdleBody: &stalledIdleBody{ctx: x.ctx, prefix: strings.NewReader(prefix)}}
			x.setBody(body)
			var reader io.Reader = body
			if scenario == "eof" {
				reader = strings.NewReader("")
			}
			_, err := parseStreamWithWatch("", reader, func(event llm.StreamEvent) error {
				if event.Kind == "text" {
					if scenario == "user" {
						cancel()
					}
					if scenario == "consumer" {
						return consumerErr
					}
				}
				return nil
			}, x.watch, x.identify)
			if scenario != "consumer" {
				err = x.watch.failure(err)
			}
			x.finish(err == nil)
			err = x.withCleanupError(err)
			if scenario == "success" {
				if err != nil || calls.Load() != 0 || !body.closed.Load() {
					t.Fatal(err, calls.Load(), body.closed.Load())
				}
				return
			}
			if err == nil || calls.Load() != 1 || strings.Contains(err.Error(), "best-effort") || !strings.Contains(err.Error(), "remote cancellation failed") {
				t.Fatal(err, calls.Load())
			}
			switch scenario {
			case "initial silence", "body timeout":
				var timeout *idleHTTPError
				if !errors.As(err, &timeout) || errors.Is(err, context.Canceled) {
					t.Fatal("timeout misclassified", err)
				}
			case "user":
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case "consumer":
				if !errors.Is(err, consumerErr) {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestAttemptCleanupCompletesBeforeNextAttempt(t *testing.T) {
	cleanupStarted, releaseCleanup := make(chan struct{}), make(chan struct{})
	provider := fakeRemoteProvider{cancel: func(ctx context.Context, id string) error {
		close(cleanupStarted)
		select {
		case <-releaseCleanup:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}}
	x := newResponseExchange(context.Background(), provider.Cancel, time.Second)
	done := make(chan struct{})
	go func() { x.finish(false); close(done) }()
	<-cleanupStarted
	select {
	case <-done:
		t.Fatal("returned before cleanup")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseCleanup)
	<-done
}

func TestRemoteCancelCleanupIsBounded(t *testing.T) {
	var hookDone sync.WaitGroup
	hookDone.Add(1)
	provider := fakeRemoteProvider{cancel: func(ctx context.Context, _ string) error { defer hookDone.Done(); <-ctx.Done(); return ctx.Err() }}
	x := newResponseExchange(context.Background(), provider.Cancel, time.Second)
	start := time.Now()
	x.finish(false)
	if elapsed := time.Since(start); elapsed < remoteCancelTimeout || elapsed > remoteCancelTimeout+time.Second {
		t.Fatal(elapsed)
	}
	hookDone.Wait()
}

func TestConcurrentAttemptsCancelOnlyTheirOwnResponse(t *testing.T) {
	var calls atomic.Int32
	provider := fakeRemoteProvider{cancel: func(ctx context.Context, id string) error {
		if id != "abandoned" {
			t.Errorf("canceled wrong attempt %q", id)
		}
		calls.Add(1)
		return nil
	}}
	abandoned := newResponseExchange(context.Background(), provider.Cancel, time.Second)
	completed := newResponseExchange(context.Background(), provider.Cancel, time.Second)
	abandoned.identify("abandoned")
	completed.identify("completed")
	var wg sync.WaitGroup
	wg.Go(func() { abandoned.finish(false) })
	wg.Go(func() { completed.finish(true) })
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}

func TestBufferedCompletionCannotReviveExpiredAttempt(t *testing.T) {
	x := newResponseExchange(context.Background(), func(context.Context, string) error { return nil }, 25*time.Millisecond)
	<-x.ctx.Done()
	_, err := parseStreamWithWatch("", strings.NewReader(sseFrames(map[string]any{"type": "response.completed"})), func(llm.StreamEvent) error {
		t.Error("published completion after expiration")
		return nil
	}, x.watch, x.identify)
	x.finish(false)
	var timeout *idleHTTPError
	if !errors.As(err, &timeout) {
		t.Fatal(err)
	}
}
