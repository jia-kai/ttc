package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"ttc/internal/history"
	"ttc/internal/render"
)

// maintainHistory finishes the startup sweep before runtime construction. Errors
// are warnings unless startup was canceled; committed deletions cannot be undone
// when asset removal or reclamation fails. Returned messages are UI-only status.
func maintainHistory(ctx context.Context, store *history.Store, protected string, progress *startupProgress, out io.Writer) ([]string, error) {
	now := time.Now()
	swept := false
	result, err := store.Maintain(ctx, now, protected, func(p history.MaintenanceProgress) {
		if p.Phase == "discovery" {
			swept = true
		}
		progress.maintenance(p)
	})
	progress.finish("History maintenance", err)
	if swept {
		label := "entries"
		if result.RemovedEntries == 1 {
			label = "entry"
		}
		if _, writeErr := fmt.Fprintf(out, "History maintenance: removed %d %s.\n", result.RemovedEntries, label); writeErr != nil {
			return nil, errors.Join(fmt.Errorf("report history maintenance (removed %d %s): %w", result.RemovedEntries, label, writeErr), ctx.Err())
		}
	}
	var notices []string
	if err != nil && ctx.Err() == nil {
		warning := "History maintenance failed: " + render.Clean(err.Error())
		if result.RemovedLineages > 0 {
			warning += fmt.Sprintf(" (removed conversation families: %d)", result.RemovedLineages)
		}
		notices = append(notices, warning)
		result.Stats, err = store.StorageStats(ctx)
		if err != nil {
			notices = append(notices, "History storage statistics unavailable: "+render.Clean(err.Error()))
		}
	}
	if canceled := ctx.Err(); canceled != nil {
		if result.RemovedLineages > 0 {
			canceled = fmt.Errorf("history maintenance interrupted (removed conversation families: %d): %w", result.RemovedLineages, canceled)
		}
		return nil, canceled
	}
	if err == nil {
		notices = append(notices, historyStorageStatus(result, now))
	}
	return notices, nil
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
	text := fmt.Sprintf("History database: %.1f %s · ~%s %s · oldest %s", size, unit, entries, entryLabel, age)
	if result.RemovedLineages > 0 {
		families := "families"
		if result.RemovedLineages == 1 {
			families = "family"
		}
		text += fmt.Sprintf(" · removed %d inactive conversation %s", result.RemovedLineages, families)
	}
	return text
}
