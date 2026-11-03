package session

import (
	"context"
	"sort"
	"sync"

	"ttc/internal/jobs"
)

// Execution identifies one live assignment/request lifecycle. IDs are never
// reused, including across explicit session generations. Retained child context
// is not an execution: each new assignment receives a new identity.
type Execution struct {
	ID         uint64
	Generation uint64
	Actor      string
	Kind       string
	JobID      string
	Phase      string // preparing, running or settling; terminal lifecycles are removed.
}

type executionRegistry struct {
	mu          sync.Mutex
	next        uint64
	active      map[uint64]Execution
	actors      map[string]uint64
	jobs        map[string]uint64
	interrupted map[uint64]bool
}

func (r *executionRegistry) begin(actor, kind, job string, generation uint64) Execution {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.beginLocked(actor, kind, job, generation)
}
func (r *executionRegistry) beginLocked(actor, kind, job string, generation uint64) Execution {
	if r.active == nil {
		r.active = map[uint64]Execution{}
		r.actors = map[string]uint64{}
		r.jobs = map[string]uint64{}
		r.interrupted = map[uint64]bool{}
	}
	r.next++
	e := Execution{ID: r.next, Generation: generation, Actor: actor, Kind: kind, JobID: job, Phase: "preparing"}
	r.active[e.ID] = e
	r.actors[actor] = e.ID
	if job != "" {
		r.jobs[job] = e.ID
	}
	return e
}
func (r *executionRegistry) finish(id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.active[id]
	if !ok {
		return
	}
	delete(r.active, id)
	delete(r.interrupted, id)
	if r.actors[e.Actor] == id {
		delete(r.actors, e.Actor)
	}
	if r.jobs[e.JobID] == id {
		delete(r.jobs, e.JobID)
	}
}

func (c *SessionCoordinator) enterMain() func() {
	generation := c.runtime.Generation()
	r := &c.executions
	r.mu.Lock()
	if r.actors["main"] != 0 {
		r.mu.Unlock()
		return func() {}
	}
	e := r.beginLocked("main", "main", "", generation)
	r.mu.Unlock()
	return func() { r.finish(e.ID) }
}

// installForegroundCancel closes the race between observed-execution cancellation
// and the worker installing its request context. Call at foreground setup, before
// admission; the registry gate is never held across UI publication or I/O.
func (r *Runtime) installForegroundCancel(cancel context.CancelCauseFunc) {
	registry := &r.coordinator.executions
	registry.mu.Lock()
	r.mu.Lock()
	r.activeCancel = cancel
	interrupted := registry.interrupted[registry.actors["main"]]
	r.mu.Unlock()
	if interrupted {
		cancel(errUserInterrupted)
	} else {
		select {
		case <-r.coordinator.stop:
			cancel(context.Canceled)
		default:
		}
	}
	registry.mu.Unlock()
}
func (r *executionRegistry) actorID(actor string) uint64 {
	if actor == "" {
		actor = "main"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.actors[actor]
}

func (r *executionRegistry) phase(id uint64) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active[id].Phase
}

func (r *executionRegistry) setPhase(id uint64, phase string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.active[id]; ok {
		e.Phase = phase
		r.active[id] = e
	}
}
func (r *executionRegistry) observeJob(v jobs.Snapshot) {
	if v.Kind != "subagent" && v.Kind != "btw" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.jobs[v.ID]
	actor := v.Owner
	switch v.Status {
	case "running":
		if id == 0 {
			if existing, ok := r.active[r.actors[actor]]; ok && existing.JobID == "" && existing.Kind == v.Kind {
				existing.JobID, existing.Phase = v.ID, "running"
				r.active[existing.ID] = existing
				r.jobs[v.ID] = existing.ID
			}
		}
	case "completed", "failed", "cancelled":
		if e, ok := r.active[id]; ok {
			delete(r.active, id)
			if r.actors[e.Actor] == id {
				delete(r.actors, e.Actor)
			}
			delete(r.jobs, v.ID)
		}
	}
}

// Executions snapshots main, coding-child and aside lifecycles using the same
// identity allocator and terminal-removal rules. No history state is restored.
func (c *SessionCoordinator) Executions() []Execution {
	r := &c.executions
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]Execution, 0, len(r.active))
	for _, e := range r.active {
		result = append(result, e)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
