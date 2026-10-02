package provider

// TransientError marks a failed request whose transport or service failure did
// not invalidate conversation data. Callers may retry after conditions change.
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }
