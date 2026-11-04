package history

import (
	"context"

	"ttc/internal/llm"
)

// ContextState is the logical main input and accounting evidence from one frozen
// selected history view. It contains no physical entry ownership or file cursors.
// The caller owns the returned messages and accounting values.
type ContextState struct {
	Messages []llm.Message
	Usage    MainUsage
}

// ReadContextState reads main input and usage at the same selected cut in one
// transaction. Later source activity cannot change the returned context. Storage
// decides how inherited entries, runtime messages and provenance are projected.
func (s *Store) ReadContextState(ctx context.Context, session string) (ContextState, error) {
	q, closeSnapshot, err := s.readSnapshot(ctx)
	if err != nil {
		return ContextState{}, err
	}
	defer closeSnapshot()
	var tip int64
	if err := q.QueryRow("SELECT coalesce(active_entry_id,0) FROM sessions WHERE id=?", session).Scan(&tip); err != nil {
		return ContextState{}, err
	}
	messages, err := messagesWith(q, session, tip)
	if err != nil {
		return ContextState{}, err
	}
	usage, err := mainUsageAt(ctx, q.reader, tip)
	if err != nil {
		return ContextState{}, err
	}
	return ContextState{Messages: messages, Usage: usage}, nil
}
