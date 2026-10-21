package jobs

import (
	"strings"
	"ttc/internal/managed"
)

func (m *Manager) scopeLocked(actor string) *managed.Scope {
	scope := m.scopes[actor]
	if scope == nil {
		scope = &managed.Scope{}
		// A previously unseen descendant must not bypass an ancestor's
		// registration fence after a concurrent ownership sweep.
		for owner, ancestor := range m.scopes {
			if strings.HasPrefix(actor, owner+"/") && ancestor.Closed() {
				scope.Seal()
				break
			}
		}
		m.scopes[actor] = scope
	}
	return scope
}

// BeginOwned opens a new assignment scope after its predecessor has been joined.
// The actor's assignment supervisor is deliberately outside this scope.
func (m *Manager) BeginOwned(actor string) {
	m.mu.Lock()
	delete(m.scopes, actor)
	m.scopeLocked(actor)
	m.mu.Unlock()
}

// RegisterOwned admits non-job work (such as a timer) to the actor's scope.
// On rejection the caller must cancel and join outside its own locks.
func (m *Manager) RegisterOwned(actor string, cancel func(), done <-chan struct{}) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.closed && m.scopeLocked(actor).Register(cancel, done)
}

// ReleaseOwned releases registration after a resource's publication fence closes.
// Actor reuse is safe: completion channels identify registrations uniquely.
func (m *Manager) ReleaseOwned(actor string, done <-chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if scope := m.scopes[actor]; scope != nil {
		scope.Release(done)
	}
}

// StopOwned cancels and joins managed work owned by an actor or descendants. The
// assignment supervisor is not a scope member. Captures remain inspectable.
func (m *Manager) StopOwned(actor string) {
	m.mu.Lock()
	var scopes []*managed.Scope
	// Ensure even an empty actor scope is sealed against concurrent launches.
	m.scopeLocked(actor)
	for owner, scope := range m.scopes {
		if owner == actor || strings.HasPrefix(owner, actor+"/") {
			scope.Seal()
			scopes = append(scopes, scope)
		}
	}
	m.mu.Unlock()
	for _, scope := range scopes {
		scope.Cancel()
	}
	for _, scope := range scopes {
		scope.Join()
	}
}
