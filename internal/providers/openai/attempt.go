package openai

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"ttc/internal/prompts"
)

const remoteCancelTimeout = 2 * time.Second

// responseExchange owns local transport, the activity clock and bounded cleanup.
// No state escapes this HTTP attempt, including across provider retries.
type responseExchange struct {
	cancelRemote  func(context.Context, string) error
	responseID    string // Protected by mu; empty before the first identifying event.
	ctx           context.Context
	watch         *idleHTTPWatch
	mu            sync.Mutex
	body          io.Closer
	success       bool
	abandoned     bool
	cleanupDone   chan struct{}
	stopCancel    func() bool
	cleanupFailed bool // Protected by mu; only a safe boolean crosses the boundary.
}

func newResponseExchange(parent context.Context, cancelRemote func(context.Context, string) error, idle time.Duration) *responseExchange {
	if idle == 0 {
		idle = responseIdleTimeout
	}
	ctx, cancel := context.WithCancel(parent)
	x := &responseExchange{cancelRemote: cancelRemote, ctx: ctx, cleanupDone: make(chan struct{})}
	x.watch = &idleHTTPWatch{parent: parent, cancel: cancel, interval: idle,
		deadline: time.Now().Add(idle), timeoutError: &idleHTTPError{interval: idle, model: true}}
	x.watch.mu.Lock()
	x.watch.timer = time.AfterFunc(idle, x.watch.expire)
	x.watch.mu.Unlock()
	x.stopCancel = context.AfterFunc(ctx, x.abandon)
	return x
}

func (x *responseExchange) identify(id string) {
	if id == "" {
		return
	}
	x.mu.Lock()
	if !x.abandoned && x.responseID == "" {
		x.responseID = id
	}
	x.mu.Unlock()
}

func (x *responseExchange) setBody(body io.Closer) {
	x.mu.Lock()
	x.body = body
	abandoned := x.abandoned
	x.mu.Unlock()
	if abandoned && body != nil {
		body.Close()
	}
}

func (x *responseExchange) abandon() {
	x.mu.Lock()
	if x.success || x.abandoned {
		x.mu.Unlock()
		return
	}
	x.abandoned = true
	body := x.body
	responseID := x.responseID
	x.mu.Unlock()
	// Abort transport before initiating remote cleanup. The remote context must
	// remain usable after both user cancellation and watchdog expiration.
	x.watch.stop()
	if body != nil {
		body.Close()
	}
	go func() {
		defer close(x.cleanupDone)
		// Consumer delivery can fail after validated terminal completion. There
		// is no longer remote work to cancel, but local transport still closes.
		if x.watch.completed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), remoteCancelTimeout)
		defer cancel()
		if err := x.cancelRemote(ctx, responseID); err != nil || ctx.Err() != nil {
			x.mu.Lock()
			x.cleanupFailed = true
			x.mu.Unlock()
		}
	}()
}

func (x *responseExchange) withCleanupError(err error) error {
	x.mu.Lock()
	failed := x.cleanupFailed
	x.mu.Unlock()
	if failed && err != nil {
		return fmt.Errorf(prompts.OpenAIRemoteCancelFailed, err)
	}
	return err
}

func (x *responseExchange) finish(success bool) {
	x.mu.Lock()
	if success && !x.abandoned {
		x.success = true
	}
	succeeded := x.success
	body := x.body
	x.mu.Unlock()
	if succeeded {
		x.stopCancel()
		x.watch.stop()
		if body != nil {
			body.Close()
		}
		return
	}
	x.abandon()
	<-x.cleanupDone // Cancel must honor its fresh deadline; join rather than detach.
}
