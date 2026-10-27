package history

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ttc/internal/llm"
	"ttc/internal/render"
)

func TestJobStopPresentationPersistsWithoutBackfill(t *testing.T) {
	s, v, turn, req := historyFixture(t)
	args := json.RawMessage(`{"job_id":"job_exact"}`)
	result := json.RawMessage(`{"job_id":"job_exact","kind":"shell","label":"Check tests","status":"cancelled"}`)
	current := render.Tool("job_stop", args, result)
	for _, md := range []render.Markdown{current, {Revision: 1, Summary: "Legacy saved summary", Detail: "Legacy saved detail"}} {
		_, calls, err := s.Assistant(v.ID, turn, "main", req, llm.Message{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: NewID("call"), Name: "job_stop", Arguments: args}}})
		if err != nil {
			t.Fatal(err)
		}
		id, err := s.CallResult(v.ID, turn, "main", calls[0], result, nil, nil, md, true)
		if err != nil {
			t.Fatal(err)
		}
		view, err := s.ReadEntryView(context.Background(), id)
		if err != nil || view.Label != md.Summary {
			t.Fatalf("saved summary: %q, want %q (%v)", view.Label, md.Summary, err)
		}
		page, err := s.InspectPage(context.Background(), id, 0, InspectionPageChars)
		if err != nil || page.Text != md.Detail {
			t.Fatalf("saved detail: %q (%v)", page.Text, err)
		}
	}
	if !strings.Contains(current.Detail, "job_exact") {
		t.Fatal(current.Detail)
	}
}
