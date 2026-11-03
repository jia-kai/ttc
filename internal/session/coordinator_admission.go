package session

import (
	"context"
	"errors"
	"fmt"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
)

const maxMainSteeringInputs = 128
const maxMainSteeringBytes = 1 << 20
const maxMainSteeringTokens = 16384

func (r *Runtime) openMainAdmission(ctx context.Context) {
	r.orderMu.Lock()
	r.coordinator.mainAccepting = ctx.Err() == nil
	r.orderMu.Unlock()
}
func (r *Runtime) sealMainAdmission() {
	r.orderMu.Lock()
	r.coordinator.mainAccepting = false
	r.orderMu.Unlock()
}

// finishMainIfIdle arbitrates the final decision with steering admission. New
// notices after sealing remain pending for another turn; late human steering
// receives the typed inactive result and is reclassified by the coordinator.
func (r *Runtime) finishMainIfIdle() bool {
	r.orderMu.Lock()
	defer r.orderMu.Unlock()
	if r.notifications.len() != 0 || len(r.steers) != 0 {
		return false
	}
	r.coordinator.mainAccepting = false
	return true
}

// checkMainSteeringCapacityLocked bounds retained human payloads, not historical
// context. Full request fit/compaction remains the admission engine's decision.
// The estimate scans at most 128 retained units, keeping accounting simple and
// correct across cancellation, batch admission and direct immutable test inputs.
func (r *Runtime) checkMainSteeringCapacityLocked(inputs []contextbuild.Input) error {
	if len(r.steers)+len(inputs) > maxMainSteeringInputs {
		return errors.New("pending steering input count is full; input was not accepted")
	}
	bytes, tokens := 0, 0
	measure := func(input contextbuild.Input) error {
		bytes += len(input.Text) + len(input.Source)
		for _, a := range input.Attachments {
			bytes += len(a.Path) + len(a.Kind) + len(a.Text)
			if a.File != nil {
				bytes += len(a.File.Path) + len(a.File.SHA256) + len(a.File.MIMEType)
			}
		}
		if bytes > maxMainSteeringBytes {
			return fmt.Errorf("pending steering payload exceeds %d bytes; input was not accepted", maxMainSteeringBytes)
		}
		estimate := contextbuild.Tokens([]llm.Message{input.Message()})
		if estimate < 0 {
			return errors.New("pending steering input has invalid token metadata")
		}
		tokens += estimate
		return nil
	}
	for _, input := range r.steers {
		if err := measure(input); err != nil {
			return err
		}
	}
	for _, input := range inputs {
		if err := measure(input); err != nil {
			return err
		}
	}
	budget := r.CurrentSelection().Model.Budget
	limit := min(maxMainSteeringTokens, max(1, budget.NextTurnInputReserve, budget.ContextLimit/4), max(1, budget.ContextLimit-budget.OutputAllowance-budget.EstimationMargin))
	if tokens > limit {
		return fmt.Errorf("pending steering exceeds its %d-token retained-input budget; input was not accepted", limit)
	}
	return nil
}
