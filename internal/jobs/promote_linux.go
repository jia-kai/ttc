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
		if j.view.Owner == actor && j.view.Kind == "shell" && j.view.Status == "running" && !j.background {
			out = append(out, j.view)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Promote transfers a foreground shell to background supervision without
// restarting or canceling it. Its existing waiter returns the current snapshot;
// an already-observed exit returns its terminal result. Wake preference is kept.
func (m *Manager) Promote(actor, id string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil || j.view.Owner != actor {
		return Snapshot{}, errors.New("unknown or inaccessible foreground shell")
	}
	if j.view.Kind != "shell" {
		return Snapshot{}, errors.New("only ordinary foreground shells can be moved to background")
	}
	if j.view.Status != "running" || j.background {
		return preview(j), nil
	}
	j.background = true
	close(j.promoted)
	return preview(j), nil
}
