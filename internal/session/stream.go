package session

import (
	"context"
	"errors"
	"fmt"

	"ttc/internal/history"
	"ttc/internal/llm"
)

// stream attaches inspection at the provider boundary without modifying model
// input. Each observation is routed into the writable continuation. A separate
// publication mutex preserves boundary batch order without holding runtime state
// locks across potentially blocking frontend delivery.
func (r *Runtime) stream(ctx context.Context, turn, actor string, request int64, input llm.Request, emit func(llm.StreamEvent) error) error {
	input.Observe = func(event llm.BoundaryEvent) error {
		// Artifact preparation is independent of the writable session cursor:
		// continuation keeps the same lineage-owned artifact directory. Do not
		// serialize large payload parsing or disk IO across active model streams.
		for attempt := 0; attempt < 3; attempt++ {
			prepared, err := r.Store.PrepareBoundary(r.Current(), request, event)
			if err != nil {
				return fmt.Errorf("prepare provider boundary: %w", err)
			}
			err = func() error {
				r.boundaryMu.Lock()
				defer r.boundaryMu.Unlock()
				r.routeMu.RLock()
				session := r.Current()
				items, err := r.Store.RecordPreparedBoundary(session, turn, actor, request, input.ConversationID, prepared)
				r.routeMu.RUnlock()
				if err != nil {
					return err
				}
				for i := range items {
					r.emit(Event{Kind: "stream_item", SessionID: session, Actor: actor, EntryID: items[i].ID, RequestID: request, StreamItem: &items[i]})
				}
				return nil
			}()
			if errors.Is(err, history.ErrBoundaryBaseChanged) {
				// Compaction or branch movement invalidated the candidate base.
				// Rebuild the immutable projection outside all publication locks.
				continue
			}
			if err != nil {
				return fmt.Errorf("record provider boundary: %w", err)
			}
			return nil
		}
		return fmt.Errorf("record provider boundary after routing retries: %w", history.ErrBoundaryBaseChanged)
	}
	return r.Provider.Stream(ctx, input, emit)
}
