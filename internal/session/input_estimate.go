package session

import (
	"context"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
)

// estimateInput applies the same canonical projection as request admission before
// asking the provider to account for its complete native input. Retained human
// inputs must be grouped here too: persisted accounting targets are not yet
// admission projections.
func (r *Runtime) estimateInput(ctx context.Context, request llm.Request) (llm.InputEstimate, error) {
	_, estimate, err := r.prepareInput(ctx, request)
	return estimate, err
}

// prepareInput returns the exact projected request admitted for streaming,
// including its immutable provider preparation. Admission may refresh canonical
// audit metadata after commit without changing the prepared native input.
func (r *Runtime) prepareInput(ctx context.Context, request llm.Request) (llm.Request, llm.InputEstimate, error) {
	messages, err := projectInputMessages(request.Selection, request.Messages)
	if err != nil {
		return llm.Request{}, llm.InputEstimate{Input: -1, Estimated: true}, err
	}
	request.Messages = messages
	estimate, err := r.Provider.EstimateInput(ctx, request)
	if err != nil {
		return llm.Request{}, estimate, err
	}
	request.Prepared = estimate.Prepared
	return request, estimate, nil
}

func projectInputMessages(selection llm.Selection, messages []llm.Message) ([]llm.Message, error) {
	return contextbuild.ProjectRetainedInputs(llm.ContextFor(selection, messages))
}
