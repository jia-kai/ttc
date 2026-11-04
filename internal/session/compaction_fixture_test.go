package session

import (
	"context"
	"time"

	"ttc/internal/history"
	"ttc/internal/llm"
)

// commitCompactionFixture constructs a continuation through the semantic API.
// Tests choose visible-message retention, never persistence entry boundaries.
func commitCompactionFixture(store *history.Store, view history.CompactionView, handoff llm.Message, start int) (history.Session, error) {
	plan, err := view.Retain(start, nil, time.Now())
	if err != nil {
		return history.Session{}, err
	}
	plan, err = store.ResolveCompaction(context.Background(), plan, handoff, nil)
	if err != nil {
		return history.Session{}, err
	}
	return store.CommitCompaction(context.Background(), plan)
}
