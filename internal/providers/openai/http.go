package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"sync"
	"time"

	"ttc/internal/prompts"
)

const responseIdleTimeout = 90 * time.Second

// idleHTTPError is safe to publish: it contains only the configured interval.
// It deliberately does not wrap context.Canceled, which denotes user/lifecycle
// cancellation to the provider retry policy.
type idleHTTPError struct {
	interval time.Duration
	model    bool
}

func (e *idleHTTPError) Error() string {
	if e.model {
		return fmt.Sprintf(prompts.OpenAIModelResponseIdle, e.interval)
	}
	return fmt.Sprintf(prompts.OpenAIHTTPResponseIdle, e.interval)
}
func (*idleHTTPError) Timeout() bool   { return true }
func (*idleHTTPError) Temporary() bool { return true }

// doIdleHTTP bounds response inactivity, not total exchange duration. Zero idle
// selects the provider default. The first response byte, informational headers,
// final headers and every nonempty body read count as activity, including bytes
// that do not form a complete SSE event. The caller must close the returned body.
// Caller/client deadlines remain effective; the supplied client is not mutated.
func doIdleHTTP(client *http.Client, req *http.Request, idle time.Duration) (*http.Response, error) {
	if idle == 0 {
		idle = responseIdleTimeout
	}
	if idle < 0 {
		return nil, errors.New("HTTP response inactivity interval must be nonnegative")
	}
	ctx, cancel := context.WithCancel(req.Context())
	watch := &idleHTTPWatch{parent: req.Context(), cancel: cancel, interval: idle, deadline: time.Now().Add(idle)}
	watch.mu.Lock()
	watch.timer = time.AfterFunc(idle, watch.expire)
	watch.mu.Unlock()
	stopParent := context.AfterFunc(req.Context(), watch.stop)
	watch.mu.Lock()
	if watch.stopped {
		watch.mu.Unlock()
		stopParent()
	} else {
		watch.stopParent = stopParent
		watch.mu.Unlock()
	}
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: watch.activity,
		Got1xxResponse:       func(int, textproto.MIMEHeader) error { watch.activity(); return nil },
	}
	ctx = httptrace.WithClientTrace(ctx, trace)
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		err = watch.failure(err)
		watch.stop()
		var idleErr *idleHTTPError
		if errors.As(err, &idleErr) && resp != nil {
			if resp.Body != nil {
				resp.Body.Close()
			}
			resp = nil // Timeout is not a caller redirect-policy rejection.
		}
		return resp, err
	}
	watch.activity()
	if err := watch.failure(nil); err != nil {
		if resp.Body != nil {
			resp.Body.Close()
		}
		watch.stop()
		return nil, err
	}
	if resp.Body == nil {
		watch.stop()
		return resp, nil
	}
	resp.Body = &idleHTTPBody{ReadCloser: resp.Body, watch: watch}
	return resp, nil
}

type idleHTTPWatch struct {
	mu               sync.Mutex
	parent           context.Context
	cancel           context.CancelFunc
	interval         time.Duration
	deadline         time.Time
	pausedAt         time.Time // Local boundary observation; excluded from inactivity accounting.
	timer            *time.Timer
	stopParent       func() bool
	stopped, expired bool
	timeoutError     error // Nil selects the raw HTTP inactivity diagnostic.
	complete         bool  // Validated model completion; delivery may still fail.
}

func (w *idleHTTPWatch) activity() {
	w.mu.Lock()
	if !w.stopped {
		w.deadline = time.Now().Add(w.interval)
	}
	w.mu.Unlock()
}

func (w *idleHTTPWatch) expire() {
	w.mu.Lock()
	if w.stopped || !w.pausedAt.IsZero() {
		w.mu.Unlock()
		return
	}
	// Updating only the deadline on reads avoids timer-heap work per chunk.
	// This check also makes an already-running timer callback safe against a
	// concurrent response arriving just before the old deadline.
	if remaining := time.Until(w.deadline); remaining > 0 {
		w.timer.Reset(remaining)
		w.mu.Unlock()
		return
	}
	w.expired = true
	w.mu.Unlock()
	w.stop()
}

// pause excludes synchronous local work without canceling transport or changing
// validated completion. It cannot revive a stopped or already overdue watch.
// A successful pause must be paired with resume by the same synchronous owner.
func (w *idleHTTPWatch) pause() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	if w.stopped || !w.pausedAt.IsZero() || !now.Before(w.deadline) {
		return false
	}
	w.pausedAt = now
	w.timer.Stop()
	return true
}

func (w *idleHTTPWatch) resume() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pausedAt.IsZero() {
		return
	}
	w.deadline = w.deadline.Add(time.Since(w.pausedAt))
	w.pausedAt = time.Time{}
	if !w.stopped {
		w.timer.Reset(time.Until(w.deadline))
	}
}

func (w *idleHTTPWatch) stop() {
	w.mu.Lock()
	w.stopped = true
	w.timer.Stop()
	stopParent := w.stopParent
	w.stopParent = nil
	w.mu.Unlock()
	if stopParent != nil {
		stopParent()
	}
	w.cancel()
}

// completeResponse marks validated model completion and stops inactivity
// accounting without aborting delivery. It cannot revive an expired watch.
func (w *idleHTTPWatch) completeResponse() {
	w.mu.Lock()
	if !w.expired {
		w.stopped = true
		w.complete = true
		w.timer.Stop()
	}
	w.mu.Unlock()
}

func (w *idleHTTPWatch) completed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.complete
}

func (w *idleHTTPWatch) failure(err error) error {
	if parentErr := w.parent.Err(); parentErr != nil {
		return parentErr
	}
	w.mu.Lock()
	expired := w.expired
	w.mu.Unlock()
	if expired {
		if w.timeoutError != nil {
			return w.timeoutError
		}
		return &idleHTTPError{interval: w.interval}
	}
	return err
}

type idleHTTPBody struct {
	io.ReadCloser
	watch *idleHTTPWatch
}

func (b *idleHTTPBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.watch.activity()
	}
	err = b.watch.failure(err)
	if err != nil {
		b.watch.stop()
	}
	return n, err
}

func (b *idleHTTPBody) Close() error {
	b.watch.stop()
	return b.ReadCloser.Close()
}
