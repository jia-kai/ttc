package llm

import (
	"errors"
	"fmt"
	"testing"
)

func TestPartialErrorPreservesFailureAndRetry(t *testing.T) {
	cause := errors.New("stream interrupted")
	transient := &TransientError{Err: cause}
	retry := Retry{Attempt: 3, MaxAttempts: 5, DelayMilliseconds: 2000, Reason: "stream interrupted after partial output"}
	partial := &PartialError{Err: transient, Retry: retry}
	wrapped := fmt.Errorf("request failed: %w", partial)
	var got *PartialError
	var gotTransient *TransientError
	if partial.Error() != cause.Error() || partial.Unwrap() != transient ||
		!errors.Is(wrapped, cause) || !errors.As(wrapped, &got) || got != partial || got.Retry != retry ||
		!errors.As(wrapped, &gotTransient) || gotTransient != transient {
		t.Fatal("partial failure lost its cause or handoff", wrapped, got, gotTransient)
	}
}

func TestContextLengthErrorPreservesFailureWithoutRetryAuthorization(t *testing.T) {
	cause := errors.New("structured provider context rejection")
	capacity := &ContextLengthError{Err: cause}
	wrapped := fmt.Errorf("request failed: %w", capacity)
	var got *ContextLengthError
	var partial *PartialError
	var transient *TransientError
	if capacity.Error() != cause.Error() || capacity.Unwrap() != cause ||
		!errors.Is(wrapped, cause) || !errors.As(wrapped, &got) || got != capacity ||
		errors.As(wrapped, &partial) || errors.As(wrapped, &transient) {
		t.Fatal("capacity failure lost its identity or authorized retry", wrapped)
	}
}
