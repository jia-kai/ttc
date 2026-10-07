// Package auth defines interactive authorization presentation independently of
// inference transports, catalog discovery and conversation history.
package auth

import "context"

// Step is a device-code prompt or status. It contains no credentials and must
// not be persisted as conversation content.
type Step struct {
	Kind           string
	URL            string
	Code           string
	Message        string
	ExpiresSeconds int
}

// UI presents authorization steps. Returning an error cancels the flow; device
// authorization itself happens out of band, not through a typed widget answer.
type UI interface {
	Present(context.Context, Step) error
}
