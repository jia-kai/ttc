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
	"ttc/internal/messagestream"
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

type replayHistory struct {
	historyReader
	replay history.Replay
	tip    int64
}

func (h *replayHistory) ReadReplay(_ context.Context, _ string, tip int64) (history.Replay, error) {
	h.tip = tip
	return h.replay, nil
}

func (*replayHistory) SubagentNames(context.Context, string) (map[string]string, error) {
	return map[string]string{"main/child_fixture": "Fixture"}, nil
}

func TestReplayConsumesTypedRowsAndForwardsFrozenCut(t *testing.T) {
	h := &replayHistory{replay: history.Replay{Name: "Typed replay", Rows: []history.ReplayRow{
		{ID: 1, Actor: "main", Message: &llm.Message{Type: llm.TypeUserInput, Content: "Input", InputTimeMS: 123}},
		{ID: 2, Actor: "main", Stream: &messagestream.Item{ID: 2, Actor: "main", Revision: 7, Title: "Saved request", Actions: []messagestream.Action{{Kind: messagestream.Inspect, Target: 7}}}},
		{ID: 3, Actor: "main/child_fixture", Label: "Saved tool · inspect", Execution: true, ToolCallID: "call"},
	}}}
	c := inspectionController(h)
	c.sidebar = newSidebar()
	c.replay(8)
	if h.tip != 8 || c.sidebar.sessionName != h.replay.Name || len(c.view.lines) != 3 {
		t.Fatalf("typed replay boundary not used: tip=%d name=%s rows=%+v", h.tip, c.sidebar.sessionName, c.view.lines)
	}
	stream, tool := c.view.lines[1], c.view.lines[2]
	if c.streamRows[2] != 1 || stream.revision != 7 || stream.inspectID != 7 || stream.action != actionInspect {
		t.Fatalf("stream display metadata lost: %+v", stream)
	}
	if tool.text != "Saved tool" || tool.callID != "call" || tool.complete || tool.action != actionExecution || !tool.markdown || !tool.brief || tool.system || tool.subagentName != "Fixture" {
		t.Fatalf("tool display metadata lost: %+v", tool)
	}
	c.replay(0)
	if h.tip != 0 {
		t.Fatalf("frontend resolved current tip outside history: %d", h.tip)
	}
}

func TestReplayPreservesUnpersistedConversation(t *testing.T) {
	c := newAsyncTestController(t, nil, nil)
	c.replay(0)
	if len(c.view.lines) != 0 || c.sidebar.sessionName != "New session" {
		t.Fatalf("blank conversation replay changed: name=%s rows=%+v", c.sidebar.sessionName, c.view.lines)
	}
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
	c.replay(0)
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
	c.replay(0)
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
