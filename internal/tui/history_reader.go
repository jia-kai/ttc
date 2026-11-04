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
	ReadReplay(context.Context, string, int64) (history.Replay, error)
	BoundaryRawPage(context.Context, int64, int, int) (history.InspectionPage, error)
	BoundaryCaptures(context.Context, int64) ([]history.BoundaryCapture, error)
	BoundaryExpandedPage(context.Context, int64, int, int) (history.InspectionPage, error)
	SubagentNames(context.Context, string) (map[string]string, error)
	ReadInspection(context.Context, int64) (history.Inspection, error)
	InspectPage(context.Context, int64, int, int) (history.InspectionPage, error)
	ReadChildAnswer(context.Context, int64) (history.ChildAnswer, error)
	Sessions(string) ([]history.Session, error)
}

var _ historyReader = (*history.Store)(nil)
