package history

import (
	"time"

	"ttc/internal/llm"
)

// continueFixture preserves explicit storage selections used by persistence
// regression fixtures, including deliberately invalid boundaries. It is test-only:
// production must select and resolve an opaque CompactionPlan instead. Every
// fixture still goes through the persistence helper's mandatory selected-tip fence.
func (s *Store) continueFixture(session string, handoff llm.Message, sourceTip, retainFrom int64, inputIDs []int64, compactedAt time.Time, pending *llm.Message) (Session, error) {
	current, err := s.Session(session)
	if err != nil {
		return Session{}, err
	}
	return s.continueAt(session, handoff, sourceTip, retainFrom, inputIDs, compactedAt, pending, current.EntryTip)
}
