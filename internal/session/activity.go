package session

import (
	"context"
	"time"

	"ttc/internal/render"
)

// startActivityRefresh protects idle loaded sessions from other instances'
// startup sweeps without scanning, deleting, or compacting history.
func (r *Runtime) startActivityRefresh() func() {
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				r.refreshLoadedActivity(ctx, now)
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (r *Runtime) refreshLoadedActivity(ctx context.Context, now time.Time) {
	if err := r.Store.RefreshLoadedActivity(ctx, now, r.Current()); err != nil && ctx.Err() == nil {
		r.emit(Event{Kind: "status", Text: "History activity refresh failed: " + render.Clean(err.Error())})
	}
}
