package session

import (
	"context"
	"time"

	"scicode/internal/render"
)

// startRetention sweeps now and every hour. The caller owns the
// returned idempotent cancel/join closure and must call it before lifecycle
// preflight/session changes, restarting after successful or failed activation.
// This keeps the loaded-session protection stable without holding UI locks
// during asset deletion. Loaded activity is refreshed for other instances' sweeps.
func (r *Runtime) startRetention() func() {
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			if _, err := r.Store.Cleanup(ctx, time.Now(), r.Current()); err != nil && ctx.Err() == nil {
				r.emit(Event{Kind: "status", Text: "History cleanup failed: " + render.Clean(err.Error())})
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}
