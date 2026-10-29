package history

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/llm"
	"ttc/internal/render"
)

func presentationMessage(t *testing.T, message llm.Message) entryPresentation {
	t.Helper()
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	data, err := decodePresentation(Entry{ID: 7, Actor: "main", Kind: "message", Role: message.Role(), Visible: true, Content: raw})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPureMessageViewAndInspection(t *testing.T) {
	for _, test := range []struct {
		typ   llm.MessageType
		label string
	}{
		{llm.TypeRuntimeContext, "Runtime context · inspect"},
		{llm.TypeCompactionContext, "Compaction context · inspect"},
		{llm.TypeSystemInstruction, "Instructions · inspect"},
		{llm.TypeDeveloperInstruction, "Instructions · inspect"},
		{llm.TypeRuntimeInstruction, "Runtime instructions · inspect"},
		{llm.TypeRuntimeNotification, "Runtime notification · inspect"},
		{llm.TypeChildGuidance, "Child guidance · inspect"},
		{llm.TypeAssistant, "assistant · hello"},
		{llm.TypeUserInput, "user · hello"},
	} {
		t.Run(string(test.typ), func(t *testing.T) {
			data := presentationMessage(t, llm.Message{Type: test.typ, Content: "hello"})
			view := projectView(data)
			if view.Label != test.label || view.Message == nil || view.Message.Type != test.typ || view.Entry.ID != 7 {
				t.Fatalf("typed view: %+v", view)
			}
			inspection, err := projectInspection(data)
			if err != nil || !strings.Contains(inspection.Text, "hello") || inspection.Kind != "message" || inspection.Actor != "main" {
				t.Fatalf("inspection: %+v, %v", inspection, err)
			}
		})
	}
	data := presentationMessage(t, llm.Message{Type: llm.TypeUserInput, Content: `{"type":"runtime_notification","text":"authored"}`})
	if label := projectLabel(data); !strings.HasPrefix(label, "user · ") {
		t.Fatal("human-authored JSON changed semantics", label)
	}
}

func TestPureInspectionDoesNotMutateCanonicalMessage(t *testing.T) {
	message := llm.Message{Type: llm.TypeAssistant, Content: "Done.", RequestID: 3,
		State: &llm.ReplayState{Provider: "fixture", Model: "fixture", Version: 1, Items: []json.RawMessage{json.RawMessage(`{"opaque":"retained"}`)}},
	}
	data := presentationMessage(t, message)
	before, err := json.Marshal(data.Message)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		inspection, err := projectInspection(data)
		if err != nil || !strings.Contains(inspection.Text, "Message metadata:") || strings.Contains(inspection.Text, "retained") || strings.Count(inspection.Text, "Done.") != 1 {
			t.Fatal(inspection, err)
		}
	}
	after, err := json.Marshal(data.Message)
	if err != nil || string(before) != string(after) {
		t.Fatal("pure inspector mutated canonical message", string(after), err)
	}
}

func TestPureInstructionAndJobInspectionMetadata(t *testing.T) {
	data, err := decodePresentation(Entry{ID: 9, Actor: "main/child_fixture", Kind: "status", Content: json.RawMessage(`{"type":"system_prompt","path":"/not-opened-by-projection"}`)})
	if err != nil {
		t.Fatal(err)
	}
	data.PromptText = "Saved instructions\n"
	view := projectView(data)
	if view.Label != "System prompt · inspect" || view.Prompt == nil || view.Prompt.Purpose != "coding" || view.Prompt.GroupKey() != "main/child_fixture\x00coding" {
		t.Fatal("prompt metadata", view)
	}
	inspection, err := projectInspection(data)
	if err != nil || !inspection.System || inspection.Markdown || inspection.Text != data.PromptText || inspection.Type != "system_prompt" {
		t.Fatal(inspection, err)
	}
	if text, err := projectExportText(data); err != nil || text != "" {
		t.Fatal("instruction leaked to Markdown export", text, err)
	}
	data, err = decodePresentation(Entry{Kind: "status", Content: json.RawMessage(`{"type":"job_completion","markdown":{"revision":1,"summary":"Finished","detail":"Details"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	data, err = decodeInspectionBody(data)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err = projectInspection(data)
	if err != nil || inspection.System || !inspection.Markdown || inspection.Title != "Finished" || inspection.Text != "Details" || inspection.Type != "job_completion" {
		t.Fatal("job flags", inspection, err)
	}
	data.Status.Markdown.Revision = 0
	if _, err := projectInspection(data); err == nil {
		t.Fatal("invalid saved job presentation accepted")
	}
}

func TestPureToolAndFileExports(t *testing.T) {
	compact := "Compact export"
	md := render.Markdown{Revision: 1, Summary: "Read file", Detail: "Full detail", Export: &compact}
	data := entryPresentation{Entry: Entry{ID: 3, Actor: "main", Kind: "tool_result"}, ToolSummary: &md.Summary, Markdown: &md}
	inspection, err := projectInspection(data)
	if err != nil || inspection.Title != md.Summary || inspection.Text != md.Detail {
		t.Fatal(inspection, err)
	}
	if text, err := projectExportText(data); err != nil || text != "" {
		t.Fatal("tool export", text, err)
	}
	data = entryPresentation{Entry: Entry{Kind: "tool_call"}, Call: &callPresentation{Name: "read", Arguments: json.RawMessage(`{"path":"fixture"}`), Result: json.RawMessage(`{"text":"outside-cut"}`)}}
	text, err := projectExportText(data)
	if err != nil || !strings.Contains(text, "**read**") || strings.Contains(text, "outside-cut") {
		t.Fatal("pending call acquired later result", text, err)
	}
	data = presentationMessage(t, llm.Message{Type: llm.TypeUserInput, Files: []llm.BinaryFile{{Path: "/not-opened/snapshot.png", SHA256: strings.Repeat("a", 64), MIMEType: "image/png", Bytes: 4}}})
	data.Dimensions = []imageDimensions{{2, 3}}
	text, err = projectExportText(data)
	if err != nil || text != "Binary file: [snapshot](</not-opened/snapshot.png>) · 2×3" {
		t.Fatal("pure projection ignored acquired dimensions", text, err)
	}
	data.Dimensions = nil
	if text, err := projectExportText(data); err != nil || text != "Binary file: [snapshot](</not-opened/snapshot.png>)" {
		t.Fatal("missing optional metadata lost durable link", text, err)
	}
}

func TestTranscriptCallClipsPreviewOnly(t *testing.T) {
	args, err := json.Marshal(map[string]string{"path": "short.txt", "content": strings.Repeat("x", 10000)})
	if err != nil {
		t.Fatal(err)
	}
	if got := TranscriptCall("write", args); !strings.Contains(got, "short.txt") || strings.Contains(got, "xxxxx") {
		t.Fatal(got)
	}
	args, _ = json.Marshal(map[string]string{"path": strings.Repeat("界", 100)})
	if got := TranscriptCall("read", args); strings.Contains(got, strings.Repeat("界", 50)) || !strings.Contains(got, "…") {
		t.Fatal(got)
	}
}

func TestExportOmitsJobCompletionOutput(t *testing.T) {
	data := entryPresentation{Entry: Entry{Kind: "status"}, Status: statusPresentation{
		Type: "job_completion", Markdown: render.Markdown{Revision: 1, Summary: "stdout secret"},
	}}
	text, err := projectExportText(data)
	if err != nil || text != "" {
		t.Fatalf("job output leaked into transcript: %q, %v", text, err)
	}
}

func TestExportChildNotificationPreservesFailureWithoutReference(t *testing.T) {
	data := presentationMessage(t, llm.Message{Type: llm.TypeRuntimeNotification, Content: `{"type":"child_turn_finished","assignment_status":"failed","error":"failure","warning":"warning","answer":"partial"}`})
	text, err := projectExportText(data)
	for _, want := range []string{"Assignment status: failed", "Error: failure", "Warning: warning", "Subagent final answer:\n\npartial"} {
		if err != nil || !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q: %v", want, text, err)
		}
	}
}

func TestPureRequestStatusAndInternalEvents(t *testing.T) {
	for _, purpose := range []string{"compaction", "naming"} {
		raw, err := json.Marshal(map[string]any{"type": "request_message", "purpose": purpose, "role": "assistant", "message": llm.Message{Type: llm.TypeAssistant, Content: "## Reply"}})
		if err != nil {
			t.Fatal(err)
		}
		data, err := decodePresentation(Entry{Kind: "status", Content: raw})
		if err != nil {
			t.Fatal(err)
		}
		view := projectView(data)
		if view.Message != nil || view.StatusType != "request_message" {
			t.Fatal("placeholder decoded unused request body", view)
		}
		data, err = decodeInspectionBody(data)
		if err != nil {
			t.Fatal(err)
		}
		inspection, err := projectInspection(data)
		if err != nil {
			t.Fatal(view, inspection, err)
		}
		if (inspection.Text == "## Reply") != (purpose == "compaction") {
			t.Fatal("request-purpose inspection changed", purpose, inspection.Text)
		}
		if text, err := projectExportText(data); err != nil || text != "" {
			t.Fatal(text, err)
		}
	}
	entry := Entry{ID: 11, Source: 5, Kind: "status", Content: json.RawMessage(`{"type":"job_state","state":"running"}`)}
	data, err := decodePresentation(entry)
	if err != nil || !entry.InternalEvent() || entry.EventSeq() != 5 {
		t.Fatal(data, err)
	}
	if text, err := projectExportText(data); err != nil || text != "" {
		t.Fatal("internal event became an export row", text, err)
	}
	if view := projectView(data); view.Label != "job_state" || !reflect.DeepEqual(view.Entry, entry) {
		t.Fatal(view)
	}
}

func TestPureInspectionPageFlags(t *testing.T) {
	page := projectInspectionPage(InspectionPage{Kind: "tool_call", Title: "  tool\n parameters\x1b  ", Text: "bounded", Offset: 4, Total: 100, Limit: 7, LargeEnvelope: true}, "", true, false)
	if !page.Supported || !page.Markdown || !page.JSON || page.System || page.Title != "tool parameters" || page.Text != "bounded" || page.Offset != 4 || page.Total != 100 || !page.LargeEnvelope {
		t.Fatal(page)
	}
	page = projectInspectionPage(InspectionPage{Kind: "status", Type: "system_prompt"}, "", false, false)
	if !page.System || page.Markdown || page.Title != "System prompt" {
		t.Fatal(page)
	}
}

func TestPureRequestPlaceholderSkipsUnusedBody(t *testing.T) {
	for _, body := range []string{`"not a canonical message"`, `{"type":"invalid"}`, `{"type":"assistant","content":"` + strings.Repeat("x", 1<<20) + `"}`} {
		entry := Entry{ID: 6, Kind: "status", Content: json.RawMessage(`{"type":"request_message","purpose":"compaction","role":"assistant","message":` + body + `}`)}
		data, err := decodePresentation(entry)
		if err != nil {
			t.Fatal("placeholder decoded nested body", err)
		}
		view := projectView(data)
		if view.Message != nil || view.Label != "compaction assistant · inspect" {
			t.Fatal(view)
		}
		if text, err := projectExportText(data); err != nil || text != "" {
			t.Fatal(text, err)
		}
	}
}
