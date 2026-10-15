package session

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"ttc/internal/history"
	"ttc/internal/render"
)

// startRetention refreshes loaded activity immediately, maintaining full history
// only at startup and on the hourly deadline preserved across worker restarts.
// The caller owns the returned idempotent cancel/join closure and must call it
// before lifecycle preflight/session changes, restarting after activation fails
// or succeeds. This keeps loaded-session protection stable during asset deletion.
// The active worker owns retentionDue; lifecycle callers transfer ownership by
// joining the old worker before starting its replacement.
func (r *Runtime) startRetention() func() {
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				r.maintainRetention(ctx, time.Now())
				if ctx.Err() != nil {
					return
				}
				timer.Reset(max(0, time.Until(r.retentionDue)))
			}
		}
	}()
	return func() { cancel(); <-done }
}

// maintainRetention runs on the retention worker, or with that worker joined.
// Attempts consume their hourly slots even on failure, avoiding expensive retries
// during lifecycle commands. A canceled initial pass remains due until it can
// publish startup statistics; canceling later hourly work keeps its next slot.
func (r *Runtime) maintainRetention(ctx context.Context, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	if now.Before(r.retentionDue) {
		if err := r.Store.RefreshLoadedActivity(ctx, now, r.Current()); err != nil && ctx.Err() == nil {
			r.emit(Event{Kind: "status", Text: "History activity refresh failed: " + render.Clean(err.Error())})
		}
		return
	}
	startup := r.retentionDue.IsZero()
	r.retentionDue = now.Add(time.Hour)
	result, err := r.Store.Maintain(ctx, now, r.Current())
	if ctx.Err() != nil {
		if startup {
			r.retentionDue = time.Time{}
		}
		return
	}
	if err != nil {
		warning := "History maintenance failed: " + render.Clean(err.Error())
		if result.RemovedLineages > 0 {
			warning += fmt.Sprintf(" (removed conversation families: %d)", result.RemovedLineages)
		}
		r.emit(Event{Kind: "status", Text: warning})
		// A purge may have committed before assets or compaction failed. Keep
		// the requested startup footprint visible even when maintenance warns.
		result.Stats, err = r.Store.StorageStats(ctx)
		if ctx.Err() != nil {
			if startup {
				r.retentionDue = time.Time{}
			}
			return
		}
		if err != nil {
			r.emit(Event{Kind: "status", Text: "History storage statistics unavailable: " + render.Clean(err.Error())})
			return
		}
	}
	// Transient view events neither admit model input nor create database entries
	// for blank conversations. Frontends consume them after initial history replay.
	r.emit(Event{Kind: "status", Text: historyStorageStatus(result, now)})
}

func historyStorageStatus(result history.MaintenanceResult, now time.Time) string {
	stats := result.Stats
	size, unit := float64(stats.Bytes), "B"
	for _, next := range []string{"KiB", "MiB", "GiB", "TiB"} {
		if size < 1024 {
			break
		}
		size /= 1024
		unit = next
	}
	entries := strconv.FormatInt(stats.Entries, 10)
	for i := len(entries) - 3; i > 0; i -= 3 {
		entries = entries[:i] + "," + entries[i:]
	}
	entryLabel := "entries"
	if stats.Entries == 1 {
		entryLabel = "entry"
	}
	age := "n/a (empty history)"
	if stats.Entries > 0 {
		days := max(0, now.Sub(time.UnixMilli(stats.OldestMS)).Hours()/24)
		age = fmt.Sprintf("%.1f days", days)
	}
	var text strings.Builder
	fmt.Fprintf(&text, "History database: %.1f %s · %s %s · oldest %s", size, unit, entries, entryLabel, age)
	if result.RemovedLineages > 0 {
		families := "families"
		if result.RemovedLineages == 1 {
			families = "family"
		}
		fmt.Fprintf(&text, " · removed %d inactive conversation %s", result.RemovedLineages, families)
	}
	return text.String()
}
