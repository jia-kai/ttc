package tui

import (
	"context"

	"ttc/internal/history"
)

// historyReader is the frontend's read-only history boundary. Runtime commands
// own mutations; replay and inspectors consume typed views rather than SQL or
// persisted JSON envelopes. InspectPage must precede a full ReadInspection.
type historyReader interface {
	PromptHistory(context.Context) ([]string, error)
	SubagentNames(context.Context, string) (map[string]string, error)
	ReadView(context.Context, history.Entry) (history.EntryView, error)
	ReadEntryView(context.Context, int64) (history.EntryView, error)
	ReadInspection(context.Context, int64) (history.Inspection, error)
	InspectPage(context.Context, int64, int, int) (history.InspectionPage, error)
	ChildResponse(int64) (int64, error)
	Sessions(string) ([]history.Session, error)
}

var _ historyReader = (*history.Store)(nil)
