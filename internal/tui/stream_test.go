package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/messagestream"
	"ttc/internal/providers/openai"
	"ttc/internal/session"
	"ttc/internal/skills"
	"ttc/internal/tool"
	"ttc/internal/workspace"
)

func TestPlainRequestRowIsOneLine(t *testing.T) {
	c := inspectionController(nil)
	var output bytes.Buffer
	c.frontend.Plain, c.frontend.Output = true, &output
	item := messagestream.Item{ID: 1, Actor: "main", Title: "Request #1 · coding · attempt 1 · completed"}
	if err := c.handleRuntimeEvent(session.Event{Kind: "stream_item", StreamItem: &item}); err != nil {
		t.Fatal(err)
	}
	if text := output.String(); strings.Count(text, "\n") != 1 || !strings.Contains(text, "Request #1") {
		t.Fatalf("plain request row is not a compact header: %q", text)
	}
}

func TestStreamRowActionsDoNotFollowCorrelationID(t *testing.T) {
	item := messagestream.Item{ID: 7, Actor: "main", Title: "Request"}
	row := streamLine(item)
	if row.id != 7 || row.action != actionNone || row.inspectID != 0 {
		t.Fatalf("identity granted inspection: %+v", row)
	}
	item.Actions = []messagestream.Action{{Kind: messagestream.Raw, Target: 10}, {Kind: messagestream.Inspect, Target: 9}}
	row = streamLine(item)
	if row.id != 7 || row.action != actionInspect || row.inspectID != 9 || row.rawID != 10 || row.text != "Request · Inspect" {
		t.Fatalf("explicit action targets not preserved: %+v", row)
	}
	for _, width := range []int{12, 40, 80} {
		view := newTranscript()
		view.append(row)
		if rows := view.viewport(width, 20); len(rows) != 1 || !strings.Contains(rows[0].text, "Request") {
			t.Fatalf("boundary card flooded width %d: %+v", width, rows)
		}
	}
}

func TestRequestRevisionsUpdateOneStableRow(t *testing.T) {
	c := inspectionController(nil)
	item := messagestream.Item{ID: 7, Revision: 10, Actor: "main", Title: "Request #1 · running", Actions: []messagestream.Action{{Kind: messagestream.Inspect, Target: 10}}}
	c.publishStream(item)
	item.Revision, item.Title, item.Actions[0].Target = 11, "Request #1 · completed", 11
	c.publishStream(item)
	if len(c.view.lines) != 1 || c.view.lines[0].id != 7 || c.view.lines[0].revision != 11 || c.view.lines[0].inspectID != 11 {
		t.Fatalf("request update appended or lost its identity: %+v", c.view.lines)
	}
	item.Revision, item.Title = 10, "stale replay"
	c.publishStream(item)
	if len(c.view.lines) != 1 || strings.Contains(c.view.lines[0].text, "stale") {
		t.Fatal("queued old revision replaced current request")
	}
}

func TestDirectRowsNeverOfferPayloadInspection(t *testing.T) {
	c := inspectionController(nil)
	for _, actor := range []string{"main", "main/child_test"} {
		c.names[actor] = ""
		for _, event := range []session.Event{
			{Kind: "message", MessageType: llm.TypeUserInput, Text: "hello", EntryID: 1},
			{Kind: "delta", RequestID: 2, Text: "answer"},
			{Kind: "assistant", RequestID: 2, Text: "answer", EntryID: 3},
			{Kind: "message", MessageType: llm.TypeRuntimeNotification, Text: `{"type":"job_exit","message":"finished"}`, EntryID: 4},
			{Kind: "status", Text: "working", EntryID: 5},
		} {
			event.Actor = actor
			if err := c.handleRuntimeEvent(event); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, row := range c.view.lines {
		if row.action != actionNone || row.inspectID != 0 || row.id < 0 {
			t.Fatalf("direct row exposed payload inspection: %+v", row)
		}
		c.openRow(row) // A persisted ID must not call the nil history reader.
	}
}

type boundaryUIProvider struct{ llm.Script }

func (*boundaryUIProvider) Stream(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
	for _, capture := range []llm.BoundaryEvent{
		{Attempt: 1, Direction: "request", Payload: json.RawMessage(`{"instructions":"line one\nline two","input":[{"role":"user","content":"hello"}]}`)},
		{Attempt: 1, Direction: "response", Payload: json.RawMessage(`{"role":"assistant","content":"answer"}`)},
		{Attempt: 1, Direction: "outcome", Payload: json.RawMessage(`{"status":"success","category":"completed"}`)},
	} {
		document, err := new(openai.Adapter).InspectBoundary(capture.Direction, capture.Payload)
		if err != nil {
			return err
		}
		capture.Inspection = &document
		if err := req.Observe(capture); err != nil {
			return err
		}
	}
	if err := emit(llm.StreamEvent{Kind: "text", Text: "answer"}); err != nil {
		return err
	}
	return emit(llm.StreamEvent{Kind: "completed"})
}

func TestUnifiedStreamLiveReloadAndReadableRawActions(t *testing.T) {
	ctx := context.Background()
	store, err := history.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	w, err := workspace.Open(t.TempDir(), store)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := skills.Discover(ctx, w.Root, "")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []session.Event
	r, err := session.New(ctx, store, w, &boundaryUIProvider{}, llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}, "", catalog, tool.WebSearchConfig{}, func(event session.Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.AutoName = false
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	r.Close() // Join runtime work before comparing the complete timeline.
	jobID, err := store.Append(r.Current(), "", "main", "status", "", false, map[string]any{
		"type": "job_completion", "markdown": map[string]string{"summary": "Saved background execution"},
	})
	if err != nil {
		t.Fatal(err)
	}
	controller := func() *frontendController {
		return &frontendController{frontend: &Frontend{Runtime: r, Output: io.Discard}, ctx: ctx, history: store, view: newTranscript(), sidebar: newSidebar(), names: map[string]string{}}
	}
	live, reload := controller(), controller()
	mu.Lock()
	defer mu.Unlock()
	events = append(events, session.Event{Kind: "job", Actor: "main", Generation: r.Generation(), EntryID: jobID, Text: "Saved background execution"})
	for _, event := range events {
		if err := live.handleRuntimeEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	reload.replay()
	boundaryRows := func(c *frontendController) []line {
		var rows []line
		for _, row := range c.view.lines {
			if row.action == actionInspect {
				rows = append(rows, row)
			} else if row.action != actionExecution && (row.action != actionNone || row.inspectID != 0) {
				t.Fatalf("direct replay row gained action: %+v", row)
			}
		}
		return rows
	}
	liveRows, savedRows := boundaryRows(live), boundaryRows(reload)
	if len(liveRows) != 1 || !reflect.DeepEqual(liveRows, savedRows) {
		t.Fatalf("boundary projections differ:\nlive=%+v\nreload=%+v", liveRows, savedRows)
	}
	for _, controller := range []*frontendController{live, reload} {
		row := controller.view.lines[len(controller.view.lines)-1]
		if row.id != jobID || row.action != actionExecution || row.inspectID != jobID {
			t.Fatalf("job completion lost execution details: %+v", row)
		}
	}
	before := len(reload.view.lines)
	item := messagestream.Item{ID: savedRows[0].id, Actor: "main", Title: "duplicate"}
	if err := reload.handleRuntimeEvent(session.Event{Kind: "stream_item", Generation: r.Generation(), StreamItem: &item}); err != nil || len(reload.view.lines) != before {
		t.Fatalf("queued publication duplicated replay: %v", err)
	}
	lastID := int64(0)
	for _, row := range reload.view.lines {
		if row.id < lastID {
			t.Fatalf("replay reordered direct/boundary rows: %+v", reload.view.lines)
		}
		lastID = row.id
	}
	reload.openRow(savedRows[0])
	if reload.modal.window == nil {
		t.Fatal("request inspector did not open")
	}
	human, nativeJSON, hasJSON := strings.Cut(reload.modal.window.Text, "Prettified raw JSON")
	if !strings.Contains(human, "line one\n") || strings.Contains(human, `line one\nline two`) {
		t.Fatalf("default inspector is not readable: %+v", reload.modal.window)
	}
	if !hasJSON || !strings.Contains(nativeJSON, `"instructions": "line one\nline two"`) {
		t.Fatalf("prettified native JSON does not follow readable details: %q", nativeJSON)
	}
	if reload.modal.window.boundaryRaw == 0 || !strings.Contains(reload.modal.window.Hint, "R raw") {
		t.Fatal("missing separate raw action")
	}
	reload.handleKey(tcell.NewEventKey(tcell.KeyRune, "r", 0))
	if !strings.Contains(reload.modal.window.Title, "Exact raw JSON") || !strings.Contains(reload.modal.window.Text, `line one\nline two`) {
		t.Fatalf("raw action did not retain exact JSON: %+v", reload.modal.window)
	}
	reload.handleKey(tcell.NewEventKey(tcell.KeyRune, "n", 0))
	if !strings.Contains(reload.modal.window.Text, "answer") || strings.Contains(reload.modal.window.Text, "line one") {
		t.Fatal("raw navigation did not select the independent response")
	}
	reload.handleKey(tcell.NewEventKey(tcell.KeyRune, "p", 0))
	if !strings.Contains(reload.modal.window.Text, `line one\nline two`) {
		t.Fatal("raw navigation did not return to outgoing request")
	}
	reload.handleKey(tcell.NewEventKey(tcell.KeyEsc, "", 0))
	reload.openRow(savedRows[0])
	reload.handleKey(tcell.NewEventKey(tcell.KeyRune, "e", 0))
	if !reload.modal.expanded || !reload.modal.window.Structured {
		t.Fatal("expanded readable view lost structured rendering")
	}
	reload.handleKey(tcell.NewEventKey(tcell.KeyRune, "e", 0))
	if reload.modal.expanded || !reload.modal.window.Structured {
		t.Fatal("compact readable view did not restore")
	}
}
