package tui

import (
	"context"

	"ttc/internal/history"
	"ttc/internal/messagestream"
)

// historyReader is the frontend's read-only history boundary. Runtime commands
// own mutations; replay and inspectors consume typed views rather than SQL or
// persisted JSON envelopes. InspectPage must precede a full ReadInspection.
type historyReader interface {
	PromptHistory(context.Context) ([]string, error)
	Branch(string, int64) ([]history.Entry, error)
	// StreamItems returns boundary rows and revisions on ancestry ending at the
	// supplied immutable entry tip, in increasing header entry-ID order.
	StreamItems(context.Context, int64) ([]messagestream.Item, error)
	BoundaryRawPage(context.Context, int64, int, int) (history.InspectionPage, error)
	BoundaryCaptures(context.Context, int64) ([]history.BoundaryCapture, error)
	BoundaryExpandedPage(context.Context, int64, int, int) (history.InspectionPage, error)
	SubagentNames(context.Context, string) (map[string]string, error)
	ReadView(context.Context, history.Entry) (history.EntryView, error)
	ExportText(history.Entry) (string, error)
	ReadInspection(context.Context, int64) (history.Inspection, error)
	InspectPage(context.Context, int64, int, int) (history.InspectionPage, error)
	ReadChildAnswer(context.Context, int64) (history.ChildAnswer, error)
	Sessions(string) ([]history.Session, error)
}

var _ historyReader = (*history.Store)(nil)
