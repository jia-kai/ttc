package llm

// TransientError marks an upstream failure that leaves existing conversation data
// usable, including after the final attempt exhausts the retry budget. The name
// does not promise another automatic attempt; callers may retry explicitly.
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// PartialError authorizes runtime-owned continuation after a provider-generated
// upstream failure with committed output. The failed request must not be replayed:
// retain partial history and settle incomplete calls before building a new request.
// Retry supplies the next attempt and cancellable wait; providers neither emit its
// notice nor wait. Local validation, callback and cancellation errors, or an
// exhausted attempt budget, must never carry this authorization.
type PartialError struct {
	Err   error // Original upstream failure; non-nil.
	Retry Retry
}

// Error preserves the original failure diagnostic.
func (e *PartialError) Error() string { return e.Err.Error() }

// Unwrap exposes the original failure for inspection.
func (e *PartialError) Unwrap() error { return e.Err }
