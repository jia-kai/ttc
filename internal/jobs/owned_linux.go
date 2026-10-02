package jobs

import "strings"

// StopOwned cancels and joins jobs owned by an actor or its descendants. The
// except ID identifies the assignment calling this method, which must not join
// itself. Success does not discard captures; Manager.Close owns their release.
func (m *Manager) StopOwned(actor, except string) {
	m.mu.Lock()
	owned := []*job{}
	for id, j := range m.jobs {
		if id != except && (j.view.Owner == actor || strings.HasPrefix(j.view.Owner, actor+"/")) {
			owned = append(owned, j)
		}
	}
	m.mu.Unlock()
	for _, j := range owned {
		j.cancel()
	}
	for _, j := range owned {
		<-j.done
	}
}
