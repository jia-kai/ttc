// Package managed provides cancellation and publication fences for transient work.
package managed

import "sync"

// Scope owns work until Close seals registration, cancels all members, and joins
// their completion fences. Completion must include callbacks and publication.
// Never call Close from a member of the scope.
type Scope struct {
	mu         sync.Mutex
	cancelOnce sync.Once
	closed     bool
	members    map[<-chan struct{}]member
}
type member struct {
	cancel func()
	done   <-chan struct{}
}

// Seal closes registration without invoking cancellation callbacks. It can be
// used under an owner's registry lock to atomically seal a tree of scopes.
func (s *Scope) Seal() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// Closed reports whether registration has been sealed.
func (s *Scope) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Register atomically admits work, or rejects it after closure. A rejected caller
// remains responsible for canceling and joining its work outside application locks.
func (s *Scope) Register(cancel func(), done <-chan struct{}) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	select {
	case <-done:
		return true
	default:
	}
	if s.members == nil {
		s.members = make(map[<-chan struct{}]member)
	}
	s.members[done] = member{cancel, done}
	return true
}

// Release removes completed work without retaining its cancellation closure.
// Call it only after closing the publication fence; concurrent cleanup may
// already have taken a snapshot, which remains safe to join.
func (s *Scope) Release(done <-chan struct{}) {
	select {
	case <-done:
	default:
		panic("managed: release before completion")
	}
	s.mu.Lock()
	delete(s.members, done)
	s.mu.Unlock()
}

func (s *Scope) snapshotLocked() []member {
	members := make([]member, 0, len(s.members))
	for _, m := range s.members {
		members = append(members, m)
	}
	return members
}

// Close is concurrent-safe and idempotent. Cancellation always precedes joining;
// no scope lock is held while invoking cancellation or waiting for completion.
func (s *Scope) Close() {
	s.Cancel()
	s.Join()
}

// Cancel seals registration and cancels all members without joining. Separate
// cancellation permits an owner to cancel sibling scopes before joining any.
func (s *Scope) Cancel() {
	s.Seal()
	s.cancelOnce.Do(func() {
		s.mu.Lock()
		members := s.snapshotLocked()
		s.mu.Unlock()
		for _, m := range members {
			select {
			case <-m.done:
			default:
				m.cancel()
			}
		}
	})
}

// Join waits for the sealed scope's completion/publication fences. Call Cancel
// first so no member can be registered after the snapshot.
func (s *Scope) Join() {
	s.mu.Lock()
	if !s.closed {
		s.mu.Unlock()
		panic("managed: join before cancel")
	}
	members := s.snapshotLocked()
	s.mu.Unlock()
	for _, m := range members {
		<-m.done
	}
}
