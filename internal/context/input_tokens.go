package context

import (
	stdcontext "context"

	"ttc/internal/llm"
)

// InputSnapshot is an immutable model-input view for local token estimation.
// Messages are canonical history; estimation projects compatible replay and
// retained inputs without mutating the referenced data.
type InputSnapshot struct {
	Selection llm.Selection
	System    string
	Tools     []llm.ToolDefinition
	Messages  []llm.Message
}

// EstimateInput estimates complete input without provider feedback or state.
// Invalid message or retained-input metadata and token overflow set Input to -1.
// Estimated is always true; no provider checkpoint is generated.
func EstimateInput(snapshot InputSnapshot) llm.InputEstimate {
	invalid := llm.InputEstimate{Input: -1, Estimated: true}
	projected, err := ProjectRetainedInputs(snapshot.Messages)
	if err != nil {
		return invalid
	}
	estimate, err := llm.EstimateInput(stdcontext.Background(), llm.Request{
		Selection: snapshot.Selection,
		System:    snapshot.System,
		Tools:     snapshot.Tools,
		Messages:  projected,
	})
	if err != nil {
		return invalid
	}
	return estimate
}

func rawTokens(messages []llm.Message) int {
	n, err := llm.EstimateMessages(stdcontext.Background(), messages)
	if err != nil {
		return -1
	}
	return n
}

// FitsInput reports whether nonnegative input tokens fit strictly below the
// context limit after output/safety reserves and optional next-turn headroom.
// Invalid input or negative budget fields do not fit. Subtraction avoids overflow.
func FitsInput(input int, budget llm.Budget, headroom bool) bool {
	remaining := budget.ContextLimit
	reserves := []int{input, budget.OutputAllowance, budget.EstimationMargin}
	if headroom {
		reserves = append(reserves, budget.NextTurnInputReserve)
	}
	for _, reserve := range reserves {
		if reserve < 0 || reserve >= remaining {
			return false
		}
		remaining -= reserve
	}
	return true
}
