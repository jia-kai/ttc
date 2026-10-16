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

// startRetention runs maintenance once at startup. The returned idempotent
// closure cancels and joins it before shutdown or a lifecycle command changes
// the loaded session. Canceled maintenance is not restarted.
func (r *Runtime) startRetention() func() {
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		r.maintainRetention(ctx, time.Now())
	}()
	return func() { cancel(); <-done }
}

// maintainRetention performs the startup sweep and publishes UI-only statistics.
func (r *Runtime) maintainRetention(ctx context.Context, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	result, err := r.Store.Maintain(ctx, now, r.Current())
	if ctx.Err() != nil {
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
