package session

import (
	"fmt"
	"strings"
	"time"
)

// TimerInspection separates immutable startup Markdown from live timer state.
type TimerInspection struct {
	Actor  string // Scheduler actor ID, for source-name resolution.
	Text   string // Prebuilt original startup parameters; unchanged by timer state.
	Header string // Plain-text current state, including a seconds countdown when scheduled.
	Focus  string // Compact status/countdown header for short or narrow viewports.
}

// TimerDetail snapshots live state without reading history or rebuilding startup
// Markdown. Countdown rounds up to seconds, or says "due now" once due. Fired
// and cancelled timers remain inspectable until reset; compaction preserves them.
// Unknown IDs return an error.
func (r *Runtime) TimerDetail(id string) (TimerInspection, error) {
	r.mu.Lock()
	w := r.timers
	r.mu.Unlock()
	if w == nil {
		return TimerInspection{}, fmt.Errorf("timer %q not found", id)
	}
	w.mu.Lock()
	v := w.items[id]
	if v == nil {
		w.mu.Unlock()
		return TimerInspection{}, fmt.Errorf("timer %q not found", id)
	}
	snapshot := *v
	w.mu.Unlock()

	var out strings.Builder
	var focus strings.Builder
	fmt.Fprintf(&focus, "Status: %s", snapshot.Status)
	fmt.Fprintf(&out, "ID: %s\nStatus: %s\nFired count: %d\n", snapshot.ID, snapshot.Status, snapshot.Fired)
	if snapshot.NextAt != "" {
		fmt.Fprintf(&out, "Next at: %s\n", snapshot.NextAt)
	}
	if snapshot.Status == "scheduled" && !snapshot.nextAt.IsZero() {
		remaining := time.Until(snapshot.nextAt)
		countdown := "due now"
		if remaining > 0 {
			seconds := (remaining-1)/time.Second + 1
			countdown = fmt.Sprintf("%d seconds", seconds)
		}
		fmt.Fprintf(&out, "Countdown: %s\n", countdown)
		fmt.Fprintf(&focus, "\nCountdown: %s", countdown)
	}
	if snapshot.Last != "" {
		fmt.Fprintf(&out, "Last result: %s\n", snapshot.Last)
	}
	return TimerInspection{Actor: snapshot.actor, Text: snapshot.startupText, Header: out.String(), Focus: focus.String()}, nil
}
