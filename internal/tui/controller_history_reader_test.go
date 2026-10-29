package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v3"

	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/session"
)

// Embedding the interface makes unrelated reads fail immediately in these
// focused tests; no SQLite store or running session is needed by an inspector.
type inspectorHistory struct {
	historyReader
	page       history.InspectionPage
	pageErr    error
	inspection history.Inspection
	reads      []string
}

func TestReplayRetainsPendingToolAndReplacesWithResult(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: []llm.ScriptResponse{
		{Calls: []llm.ToolCall{{ID: "q", Name: "question", Arguments: []byte(`{"questions":[{"id":"q","prompt":"Choose?","options":[{"id":"a","label":"Yes"},{"id":"b","label":"No"}]}]}`)}}},
		{Text: "Finished."},
	}})
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	u.wait(t, "Choose?")
	c := inspectionController(u.runtime.Store)
	c.frontend.Runtime = u.runtime
	c.sidebar = newSidebar()
	c.replay()
	var pending line
	for _, item := range c.view.lines {
		if item.callID != "" {
			if pending.callID != "" {
				t.Fatal("duplicate pending call", c.view.lines)
			}
			pending = item
		}
	}
	if pending.callID == "" || !strings.Contains(pending.text, "question") || strings.Contains(pending.text, "tool_call") || pending.action != actionExecution || pending.complete {
		t.Fatalf("pending call not rendered as inspectable execution: %+v", pending)
	}
	c.inspectSaved(pending.id)
	if c.modal.window == nil || !strings.Contains(c.modal.window.Text, "Choose?") {
		t.Fatalf("pending inspection lost saved details: %+v", c.modal.window)
	}
	updated := pending
	updated.text = "updated"
	if !c.view.publish(updated, "", false) || len(c.view.lines) == 0 || c.view.lines[c.view.calls[pending.callID]].text != "updated" {
		t.Fatal("live updates did not replace replayed pending row")
	}
	u.key(tcell.KeyEnter)
	u.wait(t, "[Submit]")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	c.replay()
	var completed []line
	for _, item := range c.view.lines {
		if item.callID == pending.callID {
			completed = append(completed, item)
		}
	}
	if len(completed) != 1 || !completed[0].complete || completed[0].id == pending.id {
		t.Fatalf("result did not replace pending call: %+v", completed)
	}
}

func TestReplayToolResultsRespectsSelectedCut(t *testing.T) {
	entries := []history.Entry{
		{ID: 1, Kind: "tool_call", Content: []byte(`{"call_id":"call"}`)},
		{ID: 2, Kind: "tool_result", Content: []byte(`{"call_id":"call"}`)},
	}
	ids, results, err := replayToolResults(entries[:1])
	if err != nil || ids[1] != "call" || results["call"] != 0 {
		t.Fatalf("result beyond replay cut affected pending intent: %v %v %v", ids, results, err)
	}
	_, results, err = replayToolResults(entries)
	if err != nil || results["call"] != 2 {
		t.Fatalf("result inside replay cut not selected: %v %v", results, err)
	}
}

func (h *inspectorHistory) InspectPage(_ context.Context, id int64, offset, limit int) (history.InspectionPage, error) {
	h.reads = append(h.reads, "page")
	h.page.ID, h.page.Offset, h.page.Limit = id, offset, limit
	return h.page, h.pageErr
}

func (h *inspectorHistory) ReadInspection(_ context.Context, _ int64) (history.Inspection, error) {
	h.reads = append(h.reads, "full")
	return h.inspection, nil
}

func inspectionController(h historyReader) *frontendController {
	return &frontendController{
		frontend: &Frontend{Runtime: &session.Runtime{}, Output: io.Discard},
		ctx:      context.Background(),
		history:  h,
		view:     newTranscript(),
		names:    map[string]string{},
	}
}

func TestInspectorPagesBeforeLoadingCompleteRecord(t *testing.T) {
	for _, largeEnvelope := range []bool{false, true} {
		t.Run(map[bool]string{false: "large body", true: "small body in large envelope"}[largeEnvelope], func(t *testing.T) {
			total := history.InspectionPageChars + 1
			if largeEnvelope {
				total = 5
			}
			h := &inspectorHistory{page: history.InspectionPage{
				Actor: "main", Kind: "message", Supported: true,
				LargeEnvelope: largeEnvelope, Total: total, Text: "Done.",
			}}
			c := inspectionController(h)
			c.inspectSaved(42)
			if got := strings.Join(h.reads, ","); got != "page" {
				t.Fatalf("reads = %s; full record must not be loaded", got)
			}
			if c.modal.window == nil || c.modal.window.pages == nil {
				t.Fatal("expected bounded paged inspector")
			}
		})
	}
}

func TestInspectorUsesTypedPresentationAfterSmallPage(t *testing.T) {
	h := &inspectorHistory{
		page: history.InspectionPage{Supported: false},
		inspection: history.Inspection{
			Actor: "main", Kind: "status", Type: "system_prompt",
			Title: "Loaded instructions", Text: "Exact instruction text", System: true,
		},
	}
	c := inspectionController(h)
	c.inspectSaved(42)
	if got := strings.Join(h.reads, ","); got != "page,full" {
		t.Fatalf("reads = %s", got)
	}
	window := c.modal.window
	if window == nil || window.Title != h.inspection.Title || window.Text != h.inspection.Text || !window.System || window.Markdown || window.SourceAgent != "" {
		t.Fatalf("inspector did not preserve typed presentation: %+v", window)
	}
}

func TestInspectorDoesNotFallThroughAfterPageError(t *testing.T) {
	h := &inspectorHistory{pageErr: errors.New("page unavailable")}
	c := inspectionController(h)
	c.inspectSaved(42)
	if got := strings.Join(h.reads, ","); got != "page" {
		t.Fatalf("reads = %s", got)
	}
	if c.modal.window != nil || len(c.view.lines) != 1 || !strings.Contains(c.view.lines[0].text, "page unavailable") {
		t.Fatal("page failure was not presented without a full read")
	}
}
