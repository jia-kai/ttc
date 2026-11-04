package history

import (
	"context"
	"errors"
	"slices"
	"time"

	ctxmgr "ttc/internal/context"
	"ttc/internal/llm"
)

// ErrCompactionPlanChanged means selected history changed after plan resolution.
// Resolve a new plan and repeat its capacity check before attempting a commit.
var ErrCompactionPlanChanged = errors.New("compaction history changed after plan resolution")

// CompactionView is a frozen main-input cut with private storage identities.
// Obtain it from ReadCompaction; its zero value cannot select a retention plan.
type CompactionView struct {
	store      *Store
	session    string
	tip        int64
	visibleIDs []int64
	messages   []llm.Message
}

// Messages returns the frozen model projection. The returned slice is owned by
// the caller; nested message data must be treated as immutable.
func (v CompactionView) Messages() []llm.Message { return slices.Clone(v.messages) }

// CompactionPlan binds retention to a frozen cut. ResolveCompaction adds the
// committed tail and replacement messages before capacity checking and commit.
// Physical entry/file identities remain private to history.
type CompactionPlan struct {
	view       CompactionView
	retainFrom int64
	inputIDs   []int64
	retained   []llm.Message
	start      int
	at         time.Time
	tip        int64
	handoff    llm.Message
	pending    *llm.Message
	messages   []llm.Message
	resolved   bool
}

// Messages returns the resolved replacement input for a capacity check. It is
// empty before resolution. The caller owns the slice; nested data is immutable.
func (p CompactionPlan) Messages() []llm.Message { return slices.Clone(p.messages) }

// ReadCompaction freezes main input and its private storage mapping in one read
// transaction. Later actor arrivals are resolved separately, never summarized
// implicitly. Empty history is rejected.
func (s *Store) ReadCompaction(ctx context.Context, session string) (CompactionView, error) {
	q, closeSnapshot, err := s.readSnapshot(ctx)
	if err != nil {
		return CompactionView{}, err
	}
	defer closeSnapshot()
	view, _, err := s.readCompactionWith(q, session)
	return view, err
}

func (s *Store) readCompactionWith(q historyReader, session string) (CompactionView, []Entry, error) {
	view := CompactionView{store: s, session: session}
	if err := q.QueryRow("SELECT coalesce(active_entry_id,0) FROM sessions WHERE id=?", session).Scan(&view.tip); err != nil {
		return CompactionView{}, nil, err
	}
	if view.tip == 0 {
		return CompactionView{}, nil, errors.New("nothing to compact")
	}
	entries, err := branchWith(q, session, view.tip, false)
	if err != nil {
		return CompactionView{}, nil, err
	}
	view.messages, err = messagesWith(q, session, view.tip)
	if err != nil {
		return CompactionView{}, nil, err
	}
	for _, entry := range entries {
		if entry.Visible {
			view.visibleIDs = append(view.visibleIDs, entry.ID)
		}
	}
	if len(view.visibleIDs) != len(view.messages) || len(entries) == 0 || entries[len(entries)-1].ID != view.tip {
		return CompactionView{}, nil, errors.New("invalid compaction projection")
	}
	return view, entries, nil
}

// Retain selects a suffix beginning at the zero-based visible-message index
// start, plus sorted unique human-input indices strictly before start. Start may
// equal len(Messages()) for an empty frozen suffix. At freezes retained-input age;
// token budgets and the choice of indices remain the caller's policy.
func (v CompactionView) Retain(start int, inputs []int, at time.Time) (CompactionPlan, error) {
	if v.store == nil || start < 0 || start > len(v.messages) || at.IsZero() || len(inputs) > 4 {
		return CompactionPlan{}, errors.New("invalid compaction retention selection")
	}
	plan := CompactionPlan{view: v, start: start, at: at}
	if start < len(v.visibleIDs) {
		plan.retainFrom = v.visibleIDs[start]
	}
	for i, input := range inputs {
		if input < 0 || input >= start || i > 0 && input <= inputs[i-1] {
			return CompactionPlan{}, errors.New("invalid compaction retained input index")
		}
		plan.inputIDs = append(plan.inputIDs, v.visibleIDs[input])
	}
	var err error
	plan.retained, err = ctxmgr.RetainedInputMessages(v.messages, inputs, at)
	if err != nil {
		return CompactionPlan{}, err
	}
	return plan, nil
}

// ResolveCompaction adds all committed arrivals after the frozen cut to a plan
// and supplies the exact replacement input for a capacity check. Handoff is the
// checked summary; pending is an already-persisted recovery instruction, if any.
// Call while excluding actor/file commits, and retain that exclusion until
// CommitCompaction. A later selected-history change causes commit to fail.
func (s *Store) ResolveCompaction(ctx context.Context, plan CompactionPlan, handoff llm.Message, pending *llm.Message) (CompactionPlan, error) {
	if plan.view.store != s || handoff.Type != llm.TypeCompactionContext || handoff.Content == "" {
		return CompactionPlan{}, errors.New("invalid compaction plan or handoff")
	}
	if err := handoff.Validate(); err != nil {
		return CompactionPlan{}, err
	}
	q, closeSnapshot, err := s.readSnapshot(ctx)
	if err != nil {
		return CompactionPlan{}, err
	}
	defer closeSnapshot()
	current, entries, err := s.readCompactionWith(q, plan.view.session)
	if err != nil {
		return CompactionPlan{}, err
	}
	found := false
	for _, entry := range entries {
		found = found || entry.ID == plan.view.tip
	}
	if !found || len(current.visibleIDs) < len(plan.view.visibleIDs) || !slices.Equal(current.visibleIDs[:len(plan.view.visibleIDs)], plan.view.visibleIDs) {
		return CompactionPlan{}, ErrCompactionPlanChanged
	}
	if plan.start == len(plan.view.visibleIDs) {
		// Global IDs can have gaps occupied by unrelated sessions. Preserve the
		// first actual post-cut entry, including hidden lifecycle/audit records.
		plan.retainFrom = current.tip + 1
		for _, entry := range entries {
			if entry.ID > plan.view.tip {
				plan.retainFrom = entry.ID
				break
			}
		}
	}
	plan.tip, plan.handoff = current.tip, handoff
	if pending != nil {
		copy := *pending
		plan.pending = &copy
	} else {
		plan.pending = nil
	}
	plan.messages = ctxmgr.CompactionReplacement(handoff, plan.retained, current.messages[plan.start:], plan.pending)
	plan.resolved = true
	return plan, ctx.Err()
}

// CommitCompaction atomically hands off a resolved, capacity-checked plan. The
// caller must exclude live actor/file commits from resolution through commit.
// Stale plans fail without freezing history or creating a continuation.
func (s *Store) CommitCompaction(ctx context.Context, plan CompactionPlan) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	if plan.view.store != s || !plan.resolved {
		return Session{}, errors.New("compaction plan was not resolved by this store")
	}
	return s.continueAt(plan.view.session, plan.handoff, plan.view.tip, plan.retainFrom, plan.inputIDs, plan.at, plan.pending, plan.tip)
}
