package history

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/llm"
	"ttc/internal/render"
)

func TestTypedViewsCarryCanonicalMessagesAndRequestMessages(t *testing.T) {
	s, v, turn, request := historyFixture(t)
	ctx := context.Background()
	for _, kind := range []string{"message", "summary"} {
		message := llm.Message{Type: llm.TypeAssistant, Content: "Saved content", RequestID: request,
			State: &llm.ReplayState{Provider: "fixture", Model: "fixture", Version: 1, Items: []json.RawMessage{json.RawMessage(`{"opaque":"replay"}`)}},
		}
		id, err := s.Append(v.ID, turn, "main", kind, message.Role(), true, message)
		if err != nil {
			t.Fatal(err)
		}
		entry, err := s.Entry(id)
		if err != nil {
			t.Fatal(err)
		}
		view, err := s.ReadView(ctx, entry)
		if err != nil || view.Message == nil || !reflect.DeepEqual(*view.Message, message) || !reflect.DeepEqual(view.Entry, entry) || view.StatusType != "" || view.Prompt != nil || view.Label != "assistant · Saved content" {
			t.Fatal("canonical typed view", view, err)
		}
		inspection, err := s.ReadInspection(ctx, id)
		if err != nil || !strings.Contains(inspection.Text, message.Content) || strings.Contains(inspection.Text, "opaque") || inspection.System || !inspection.Markdown {
			t.Fatal(inspection, err)
		}
	}
	request, err := s.StartRequest(v.ID, turn, "main", "compaction", v.Model)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := s.RequestMessages(v.ID, turn, "compaction", request, []llm.Message{{Type: llm.TypeAssistant, Content: "## Summary"}})
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.ReadEntryView(ctx, ids[0])
	if err != nil || view.StatusType != "request_message" || view.Message != nil || view.Label != "compaction assistant · inspect" {
		t.Fatal(view, err)
	}
	inspection, err := s.ReadInspection(ctx, ids[0])
	if err != nil || inspection.Text != "## Summary" || !inspection.System || !inspection.Markdown {
		t.Fatal(inspection, err)
	}
}

func TestTypedPromptViewGroupsWithoutLoadingInstructions(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	ctx := context.Background()
	for _, test := range []struct {
		actor, purpose string
		withRequest    bool
	}{
		{"main", "coding", false},
		{"main", "compaction", true},
		{"main/child_fixture", "naming", true},
	} {
		var request int64
		var err error
		if test.withRequest {
			request, err = s.StartRequest(v.ID, turn, test.actor, test.purpose, v.Model)
			if err != nil {
				t.Fatal(err)
			}
		}
		id, err := s.RecordSystemPrompt(v.ID, turn, test.actor, request, "Exact saved instructions\n"+test.purpose)
		if err != nil {
			t.Fatal(err)
		}
		inspection, err := s.ReadInspection(ctx, id)
		if err != nil || inspection.Actor != test.actor || inspection.Type != "system_prompt" || !inspection.System || inspection.Markdown || inspection.Text != "Exact saved instructions\n"+test.purpose {
			t.Fatal(inspection, err)
		}
		view, err := s.ReadEntryView(ctx, id)
		if err != nil || view.StatusType != "system_prompt" || view.Prompt == nil || view.Message != nil {
			t.Fatal(view, err)
		}
		prompt := view.Prompt
		if prompt.Actor != test.actor || prompt.Purpose != test.purpose || prompt.RequestID != request || prompt.Path == "" || prompt.GroupKey() != test.actor+"\x00"+test.purpose {
			t.Fatal("prompt dedup metadata", prompt)
		}
		if err := os.Remove(prompt.Path); err != nil {
			t.Fatal(err)
		}
		if view, err := s.ReadEntryView(ctx, id); err != nil || view.Prompt.Path != prompt.Path || view.Label != "System prompt · inspect" {
			t.Fatal("replay tried to read instruction file", view, err)
		}
		if _, err := s.ReadInspection(ctx, id); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("required instruction read did not fail", err)
		}
	}
}

func TestTypedToolAndJobViewsAndFrozenExportReader(t *testing.T) {
	s, v, turn, request := historyFixture(t)
	ctx := context.Background()
	call, err := s.CallIntent(v.ID, turn, "main", request, llm.ToolCall{ID: "provider", Name: "read", Arguments: json.RawMessage(`{"path":"fixture"}`)})
	if err != nil {
		t.Fatal(err)
	}
	md := render.Markdown{Revision: 1, Summary: "Small summary", Detail: strings.Repeat("界", 1<<18)}
	id, err := s.CallResult(v.ID, turn, "main", call, json.RawMessage(`{"ok":true}`), nil, nil, md, true)
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.ReadEntryView(ctx, id)
	if err != nil || view.Label != md.Summary || view.Message != nil || view.StatusType != "" {
		t.Fatal("tool replay view", view, err)
	}
	page, err := s.InspectPage(ctx, id, 0, 7)
	if err != nil || page.Text != strings.Repeat("界", 7) || page.Total != 1<<18 {
		t.Fatal("tool paging changed", page, err)
	}
	inspection, err := s.ReadInspection(ctx, id)
	if err != nil || inspection.Title != md.Summary || inspection.Text != md.Detail || inspection.System || !inspection.Markdown {
		t.Fatal("tool typed inspection", inspection.Title, len(inspection.Text), err)
	}

	q, close, err := s.transcriptSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	// Establish a snapshot, then change live data to detect accidental use of s.db.
	var count int
	if err := q.QueryRow("SELECT count(*) FROM tool_records").Scan(&count); err != nil {
		t.Fatal(err)
	}
	changed, err := json.Marshal(render.Markdown{Revision: 1, Summary: "Changed", Detail: "Outside snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE tool_records SET markdown_json=? WHERE entry_id=?", string(changed), id); err != nil {
		t.Fatal(err)
	}
	if text, err := s.exportTextWith(ctx, q, view.Entry); err != nil || text != "" {
		t.Fatal("export escaped frozen reader", len(text), err)
	}
	if text, err := s.ExportText(view.Entry); err != nil || text != "" {
		t.Fatal("live export control", text, err)
	}

	jobID, err := s.Append(v.ID, turn, "main/child_fixture", "status", "", false, map[string]any{"type": "job_completion", "markdown": render.Markdown{Revision: 1, Summary: "Job finished", Detail: "Output"}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.ReadEntryView(ctx, jobID)
	if err != nil || job.Label != "Job finished" || job.StatusType != "job_completion" {
		t.Fatal(job, err)
	}
	inspection, err = s.ReadInspection(ctx, jobID)
	if err != nil || inspection.Type != "job_completion" || inspection.System || !inspection.Markdown || inspection.Text != "Output" || inspection.Actor != "main/child_fixture" {
		t.Fatal(inspection, err)
	}
}

func TestTypedReadErrorsAndOptionalImageMetadata(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	ctx := context.Background()
	for _, read := range []func() error{
		func() error { _, err := s.ReadEntryView(ctx, -1); return err },
		func() error { _, err := s.ReadInspection(ctx, -1); return err },
		func() error { _, err := s.ReadView(ctx, Entry{ID: -1, Kind: "tool_result"}); return err },
	} {
		if err := read(); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "entry #-1") {
			t.Fatal("missing typed read lacked history identity/context", err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, read := range []func() error{
		func() error { _, err := s.ReadEntryView(canceled, v.EntryTip); return err },
		func() error { _, err := s.ReadInspection(canceled, v.EntryTip); return err },
		func() error { _, err := s.ReadView(canceled, Entry{}); return err },
	} {
		if err := read(); !errors.Is(err, context.Canceled) {
			t.Fatal("typed read ignored cancellation", err)
		}
	}
	id, err := s.Append(v.ID, turn, "main", "message", "user", true, llm.Message{Type: llm.TypeUserInput, Files: []llm.BinaryFile{{Path: "/missing/optional-snapshot.png", SHA256: strings.Repeat("a", 64), MIMEType: "image/png", Bytes: 4}}})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.Entry(id)
	if err != nil {
		t.Fatal(err)
	}
	if text, err := s.ExportText(entry); err != nil || text != "Binary file: [snapshot](</missing/optional-snapshot.png>)" {
		t.Fatal("optional dimensions made export fail", text, err)
	}
	if _, err := s.ReadView(ctx, Entry{ID: 5, Kind: "message", Content: json.RawMessage(`{"type":"invalid"}`)}); err == nil || !strings.Contains(err.Error(), "entry #5") {
		t.Fatal("invalid canonical message accepted", err)
	}
}

func TestTypedRequestPlaceholderAndMissingToolRecord(t *testing.T) {
	s, v, turn, request := historyFixture(t)
	ctx := context.Background()
	id, err := s.Append(v.ID, turn, "main", "status", "", false, json.RawMessage(`{"type":"request_message","purpose":"compaction","role":"assistant","message":"invalid unused body"}`))
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.ReadEntryView(ctx, id)
	if err != nil || view.Label != "compaction assistant · inspect" || view.Message != nil {
		t.Fatal("placeholder decoded request body", view, err)
	}
	if text, err := s.ExportText(view.Entry); err != nil || text != "" {
		t.Fatal("export decoded request body", text, err)
	}
	if _, err := s.ReadInspection(ctx, id); err == nil {
		t.Fatal("inspector accepted invalid required request body")
	}
	call, err := s.CallIntent(v.ID, turn, "main", request, llm.ToolCall{ID: "provider", Name: "read", Arguments: json.RawMessage(`{"path":"fixture"}`)})
	if err != nil {
		t.Fatal(err)
	}
	// A missing record violates CallResult's atomic commit. Do not silently
	// synthesize a different presentation from the mutable call row.
	id, err = s.Append(v.ID, turn, "main", "tool_result", "tool", true, map[string]string{"call_id": call})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadInspection(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing tool record acquired a fallback", err)
	}
	if _, err := s.ReadEntryView(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing tool summary acquired a fallback", err)
	}
}

func TestChildResponseLookupDoesNotLoadLargeAssistantEnvelopes(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	response, err := s.Append(v.ID, turn, "main/child_fixture", "message", "assistant", false,
		llm.Message{Type: llm.TypeAssistant, Content: strings.Repeat("x", 4<<20)})
	if err != nil {
		t.Fatal(err)
	}
	finish, err := json.Marshal(map[string]any{"type": "child_turn_finished", "child_id": "main/child_fixture", "result_entry_id": response})
	if err != nil {
		t.Fatal(err)
	}
	notice, err := s.Append(v.ID, turn, "main", "message", "user", true, llm.Message{Type: llm.TypeRuntimeNotification, Content: string(finish)})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ id, want int64 }{{response, 0}, {notice, response}} {
		result := testing.Benchmark(func(b *testing.B) {
			for range b.N {
				got, err := s.ChildResponse(test.id)
				if err != nil || got != test.want {
					b.Fatal(got, err)
				}
			}
		})
		if bytes := result.AllocedBytesPerOp(); bytes > 1<<20 {
			t.Fatalf("lookup #%d allocated %d bytes/op for a metadata-only read", test.id, bytes)
		}
	}
}
