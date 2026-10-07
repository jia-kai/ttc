package llm

// TransientError marks a failed request whose transport or service failure did
// not invalidate conversation data. Callers may retry after conditions change.
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// PartialError authorizes runtime-owned continuation after a provider-generated
// transient failure with committed output. The failed request must not be replayed:
// retain partial history and settle incomplete calls before building a new request.
// Retry supplies the next attempt and cancellable wait; providers neither emit its
// notice nor wait. Callback, cancellation, protocol and permanent errors, or an
// exhausted attempt budget, must never carry this authorization.
type PartialError struct {
	Err   error // Original transient failure; non-nil.
	Retry Retry
}

// Error preserves the original failure diagnostic.
func (e *PartialError) Error() string { return e.Err.Error() }

// Unwrap exposes the original failure for inspection.
func (e *PartialError) Unwrap() error { return e.Err }
