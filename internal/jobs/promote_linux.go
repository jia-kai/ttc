package jobs

import (
	"errors"
	"sort"
)

// Foreground returns copied metadata for an actor's running foreground shells.
// Coding-child assignments and LSP servers cannot be promoted by this API.
func (m *Manager) Foreground(actor string) []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Snapshot{}
	for _, j := range m.jobs {
		if j.view.Owner == actor && j.view.Kind == "shell" && j.view.Status == "running" && !j.view.Background {
			out = append(out, cloneSnapshot(j.view))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Promote transfers a foreground shell to background supervision without
// restarting or canceling it. Its existing waiter returns the current snapshot;
// an already-observed exit returns its terminal result. The deadline is preserved.
func (m *Manager) Promote(actor, id string) (Snapshot, error) {
	m.mu.Lock()
	j := m.jobs[id]
	if j == nil || j.view.Owner != actor {
		m.mu.Unlock()
		return Snapshot{}, errors.New("unknown or inaccessible foreground shell")
	}
	if j.view.Kind != "shell" {
		m.mu.Unlock()
		return Snapshot{}, errors.New("only ordinary foreground shells can be moved to background")
	}
	// Cancellation already owns this foreground process. Do not reassign its
	// terminal delivery to the background callback while its waiter is joining.
	if j.view.Status == "running" && !j.view.Background && j.ctx.Err() != nil {
		done := j.done
		m.mu.Unlock()
		<-done
		return m.View(actor, id)
	}
	defer m.mu.Unlock()
	if j.view.Status != "running" || j.view.Background {
		return preview(j), nil
	}
	j.view.Background = true
	j.view.Promoted = true
	view := preview(j)
	close(j.promoted)
	return cloneSnapshot(view), nil
}
