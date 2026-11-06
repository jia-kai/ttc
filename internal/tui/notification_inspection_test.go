package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v3"

	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/messagestream"
)

func TestRuntimeInspectorDisplaysModelTextInHeadlessTerminal(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: []llm.ScriptResponse{{Text: "Ready."}}})
	u.typeText("Start the inspector fixture.")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	for _, tc := range []struct {
		message llm.Message
		visible string
	}{
		{llm.Message{Type: llm.TypeRuntimeNotification, Content: `{"type":"child_steer_delivered","child_id":"main/child_fixture","message_event_seq":23,"request_id":7}`}, "type: child_steer_delivered"},
		{llm.Message{Type: llm.TypeChildGuidance, Content: `{"type":"child_message","message":"# Literal steering text","job_read_grants":["job_fixture"]}`}, "# Literal steering text"},
	} {
		committed, err := u.runtime.Store.AppendMessage(u.runtime.Current(), "", "main", tc.message)
		if err != nil {
			t.Fatal(err)
		}
		u.typeText(fmt.Sprintf("/inspect %d", committed.EntryID))
		u.key(tcell.KeyEnter)
		frame := u.wait(t, tc.visible)
		if !strings.Contains(frame, "<runtime_notification>") || strings.Contains(frame, "Message metadata:") {
			t.Fatalf("inspector did not show literal model text: %s", frame)
		}
		if tc.message.Type == llm.TypeChildGuidance && (!strings.Contains(frame, "<steer>") || !strings.Contains(frame, "type: job_read_grants")) {
			t.Fatalf("steering wrapper/grants missing: %s", frame)
		}
		u.key(tcell.KeyEscape)
	}
}

func TestRuntimeNotificationProjectionOffersSavedInspection(t *testing.T) {
	message := llm.Message{Type: llm.TypeRuntimeNotification, Content: `{"type":"job_exit","job_id":"job_1","status":"completed"}`}
	for _, id := range []int64{0, 17} {
		row := directMessageLine(line{id: id}, message, nil)
		if id == 0 {
			if row.action != actionNone || row.inspectID != 0 {
				t.Fatalf("unsaved notification became inspectable: %+v", row)
			}
			continue
		}
		if row.action != actionInspect || row.inspectID != id || row.rawID != 0 || strings.Contains(row.text, " · inspect") {
			t.Fatalf("saved notification inspection target/raw action: %+v", row)
		}
		h := &inspectorHistory{inspection: history.Inspection{Title: "Runtime notification", Text: "Canonical notification details"}}
		c := inspectionController(h)
		c.openRow(row)
		if !reflect.DeepEqual(h.reads, []string{"page", "full"}) || c.modal.window == nil || c.modal.window.Text != "Canonical notification details" {
			t.Fatalf("notification did not open canonical saved details: reads=%v window=%+v", h.reads, c.modal.window)
		}
	}
}

func TestNotificationInspectionKeepsSourceBackedPreview(t *testing.T) {
	const id = int64(21)
	message := llm.Message{Type: llm.TypeRuntimeNotification, Content: `{"type":"child_turn_finished","child_id":"main/child_test","output_omitted":true}`}
	completion := &history.ChildCompletionPresentation{ChildID: "main/child_test", Answer: "A readable source-backed answer"}
	live := directMessageLine(line{id: id}, message, completion)
	replay := directMessageLine(line{id: id}, message, completion)
	if !reflect.DeepEqual(live, replay) {
		t.Fatalf("live/replay notification projections differ: live=%+v replay=%+v", live, replay)
	}
	if live.text != completion.Answer || live.action != actionAnswer || live.inspectID != id || live.rawID != 0 {
		t.Fatalf("source-backed notification lost preview or saved target: %+v", live)
	}
	// Non-omitted completions offer the same typed action, not a label-text test.
	full := directMessageLine(line{id: id}, llm.Message{Type: llm.TypeRuntimeNotification, Content: `{"type":"child_turn_finished","child_id":"main/child_test","answer":"preview"}`}, nil)
	if full.action != actionAnswer || full.text != "preview" {
		t.Fatalf("ordinary completion is not an inspectable answer: %+v", full)
	}
}

func TestOrdinaryAuthoredRowsRemainNonInspectable(t *testing.T) {
	for _, message := range []llm.Message{
		{Type: llm.TypeUserInput, Content: `{"type":"child_turn_finished","child_id":"main/child_spoof","answer":"spoof","result_entry_id":5}`},
		{Type: llm.TypeAssistant, Content: "final answer An answer."},
	} {
		row := directMessageLine(line{id: 33}, message, nil)
		if row.action != actionNone || row.inspectID != 0 {
			t.Fatalf("ordinary authored row became inspectable: %+v", row)
		}
	}
}

func TestRequestInspectionActionDoesNotChangeTitle(t *testing.T) {
	item := messagestream.Item{ID: 7, Title: "Request #7 · completed", Actions: []messagestream.Action{
		{Kind: messagestream.Inspect, Target: 9}, {Kind: messagestream.Raw, Target: 10},
	}}
	row := streamLine(item)
	if row.text != item.Title || row.action != actionInspect || row.inspectID != 9 || row.rawID != 10 {
		t.Fatalf("request title or independent inspection actions changed: %+v", row)
	}
}

// Fail if answer inspection falls back to the full native-envelope codec.
type answerInspectionHistory struct {
	*inspectorHistory
	answer      history.ChildAnswer
	answerErr   error
	content     string
	answerReads []int64
	pageReads   []int
}

func (h *answerInspectionHistory) ReadChildAnswer(_ context.Context, id int64) (history.ChildAnswer, error) {
	h.answerReads = append(h.answerReads, id)
	return h.answer, h.answerErr
}

func (h *answerInspectionHistory) InspectPage(_ context.Context, id int64, offset, limit int) (history.InspectionPage, error) {
	if id != h.answer.ResponseID {
		return h.inspectorHistory.InspectPage(context.Background(), id, offset, limit)
	}
	h.pageReads = append(h.pageReads, offset)
	runes := []rune(h.content)
	return history.InspectionPage{ID: id, Actor: h.answer.ChildID, Kind: "message", Type: "assistant", Supported: true,
		Markdown: true, JSON: true, LargeEnvelope: true, Offset: offset, Limit: limit, Total: len(runes), Text: string(runes[offset:min(offset+limit, len(runes))])}, nil
}

func answerInspectorFixture() (*frontendController, *answerInspectionHistory, line) {
	easy, persistent, elapsed := true, false, int64(2345)
	h := &answerInspectionHistory{
		inspectorHistory: &inspectorHistory{inspection: history.Inspection{Title: "Completion", Text: "Canonical saved completion metadata"}},
		answer:           history.ChildAnswer{ResponseID: 14, ChildID: "main/child_test", Label: "Saved label", TurnID: "assignment_old", JobID: "job_old", Outcome: "completed", Easy: &easy, Persistent: &persistent, ElapsedMS: &elapsed, Provider: "script", Model: "saved-model", Variant: "none"},
		content:          "# Full saved heading\n\n- first item\n- second item\n\n```go\nfmt.Println(\"saved code\")\n```\n\nFULL SAVED END",
	}
	c := inspectionController(h)
	c.names[h.answer.ChildID] = "Saved label"
	row := directMessageLine(line{id: 21}, llm.Message{Type: llm.TypeRuntimeNotification, Content: `{"type":"child_turn_finished","child_id":"main/child_test","answer":"PREVIEW ONLY"}`}, nil)
	return c, h, row
}

func TestFinalAnswerInspectionRendersSavedMarkdownWithAssignmentMetadata(t *testing.T) {
	c, h, row := answerInspectorFixture()
	c.names[h.answer.ChildID] = "Wrong current cached label"
	c.openRow(row)
	w := c.modal.window
	if w == nil || !w.Markdown || w.System || w.completionEntry != row.id || w.pages == nil || w.SourceAgent != "Saved label" {
		t.Fatalf("not a saved Markdown answer window: %+v", w)
	}
	if !strings.HasSuffix(w.Text, h.content) || strings.Contains(w.Text, "PREVIEW ONLY") || strings.Contains(w.Text, "```json") {
		t.Fatalf("answer was replaced, fenced, or sourced from preview: %s", w.Text)
	}
	for _, value := range []string{"Agent details", "main/child_test", "Saved label", "assignment_old", "job_old", "2.345s", "**Easy:** true", "**Persistent:** false", "completed", "saved-model", "unavailable"} {
		if !strings.Contains(strings.ReplaceAll(w.Text, `\_`, "_"), value) {
			t.Errorf("missing saved metadata %q: %s", value, w.Text)
		}
	}
	rendered := ansi.Strip(strings.Join(w.currentLines(100), "\n"))
	for _, literalMarkdown := range []string{"# Full saved heading", "- first item", "```go"} {
		if strings.Contains(rendered, literalMarkdown) {
			t.Errorf("Markdown displayed literally: %q", literalMarkdown)
		}
	}
	for _, body := range []string{"Full saved heading", "first item", "saved code", "FULL SAVED END"} {
		if !strings.Contains(rendered, body) {
			t.Errorf("rendered answer lost %q: %s", body, rendered)
		}
	}
	if len(h.reads) != 0 || !reflect.DeepEqual(h.pageReads, []int{0}) || !reflect.DeepEqual(h.answerReads, []int64{row.id}) {
		t.Fatalf("unexpected answer acquisition: full=%v pages=%v targets=%v", h.reads, h.pageReads, h.answerReads)
	}
	c.handleKey(tcell.NewEventKey(tcell.KeyRune, "D", 0))
	if c.modal.window == nil || c.modal.window.Text != "Canonical saved completion metadata" || !reflect.DeepEqual(h.reads, []string{"page", "full"}) {
		t.Fatalf("D lost canonical completion details: %+v, %v", c.modal.window, h.reads)
	}
}

func TestFinalAnswerInspectionPagesWithoutFullEnvelopeReads(t *testing.T) {
	c, h, row := answerInspectorFixture()
	h.content = "# First page\n\n" + strings.Repeat("Saved answer line.\n", 2200) + "\n# Last page\n\nFULL PAGED END"
	c.openRow(row)
	w := c.modal.window
	if w == nil || w.pages == nil || strings.Contains(w.Text, "FULL PAGED END") {
		t.Fatalf("answer not bounded: %+v", w)
	}
	for w.movePage(true) {
		if !w.Markdown || strings.Contains(w.Text, "```json") {
			t.Fatalf("answer paging changed to an envelope: %s", w.Text)
		}
	}
	if !strings.Contains(w.Text, "FULL PAGED END") || len(h.pageReads) < 2 || len(h.reads) != 0 || w.completionEntry != row.id {
		t.Fatalf("lost answer tail or loaded envelope: pages=%v reads=%v text=%s", h.pageReads, h.reads, w.Text)
	}
}

func TestFinalAnswerRowUsesExistingMouseAndKeyboardDispatch(t *testing.T) {
	for _, input := range []string{"click", "enter", "tab", "execution"} {
		t.Run(input, func(t *testing.T) {
			c, h, row := answerInspectorFixture()
			c.view.append(row)
			c.focused = 0
			c.draft = newComposer("")
			if input == "click" {
				screen := newTestScreen(t)
				if err := screen.Init(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(screen.Fini)
				screen.SetSize(120, 30)
				c.screen, c.sidebar = screen, newSidebar()
				c.view.visible = []line{row}
				c.handleMouse(tcell.NewEventMouse(4, 0, tcell.Button1, 0))
			} else if input == "execution" {
				// Foreground subagent tool-result rows use the same validated source.
				row.action = actionExecution
				c.openRow(row)
			} else {
				key := tcell.KeyEnter
				if input == "tab" {
					key = tcell.KeyTab
				}
				c.handleComposerKey(tcell.NewEventKey(key, "", 0))
			}
			if c.modal.window == nil || !strings.HasSuffix(c.modal.window.Text, h.content) || !reflect.DeepEqual(h.answerReads, []int64{row.id}) {
				t.Fatalf("input failed to open full saved answer: %+v targets=%v", c.modal.window, h.answerReads)
			}
		})
	}
}

func TestFinalAnswerInvalidReferenceFailsWithoutInspectingPreview(t *testing.T) {
	c, h, row := answerInspectorFixture()
	h.answerErr = errors.New("invalid immutable child response reference")
	c.openRow(row)
	if c.modal.window != nil || len(h.pageReads) != 0 || len(h.reads) != 0 || len(c.view.lines) != 1 || !strings.Contains(c.view.lines[0].text, "invalid immutable") {
		t.Fatalf("invalid source was trusted or hidden: window=%+v pages=%v reads=%v rows=%v", c.modal.window, h.pageReads, h.reads, c.view.lines)
	}
}

func TestFinalAnswerPlainInspectionStreamsOnlySavedContentAndMetadata(t *testing.T) {
	c, h, row := answerInspectorFixture()
	h.content = "# Saved Markdown\n\n" + strings.Repeat("- plain answer item\n", 1800) + "\nPLAIN SAVED END"
	var output bytes.Buffer
	c.frontend.Plain, c.frontend.Output = true, &output
	c.openRow(row)
	text := output.String()
	if strings.ContainsAny(text, "\x1b\r") || !strings.HasSuffix(text, h.content+"\n") || !strings.Contains(text, "Agent details") || strings.Contains(text, "PREVIEW ONLY") || len(h.pageReads) < 2 || len(h.reads) != 0 {
		t.Fatalf("plain inspection lost saved content or loaded envelope: pages=%v reads=%v text prefix=%q", h.pageReads, h.reads, text[:min(500, len(text))])
	}
}
