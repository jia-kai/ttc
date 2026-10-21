package context

import (
	stdcontext "context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

func TestRecentCycleRetentionAndBudgets(t *testing.T) {
	m := []llm.Message{{Type: llm.TypeUserInput, Content: strings.Repeat("a", 600)}, {Type: llm.TypeAssistant, Content: "old"}, {Type: llm.TypeUserInput, Content: "new"}, {Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: "c", Name: "read", Arguments: []byte(`{"path":"x"}`)}}}, {Type: llm.TypeToolResult, CallID: "c", Content: `{"ok":true}`}}
	cut, e := Retain(m, 0, 200)
	if e != nil || cut.Start != 3 || !reflect.DeepEqual(cut.Inputs, []int{0, 2}) {
		t.Fatalf("cut=%+v err=%v", cut, e)
	}
	sel := llm.Selection{Model: llm.ScriptModel()}
	if !Fits(sel, "instructions", nil, m, false) {
		t.Fatal("small request did not fit")
	}
	if Fits(sel, strings.Repeat("a", 100000), nil, m, false) {
		t.Fatal("oversize request fit")
	}
}

func TestRetentionKeepsFittingRecentCyclesAndBalancedParallelTools(t *testing.T) {
	messages := []llm.Message{
		{Type: llm.TypeUserInput, Content: "Keep researching."},
		{Type: llm.TypeAssistant, Content: strings.Repeat("old ", 1000), Calls: []llm.ToolCall{{ID: "a"}, {ID: "b"}}},
		{Type: llm.TypeToolResult, CallID: "b", Content: "second result"},
		{Type: llm.TypeToolResult, CallID: "a", Content: "first result"},
		{Type: llm.TypeAssistant, Content: "Recent reasoning", Calls: []llm.ToolCall{{ID: "c"}}},
		{Type: llm.TypeToolResult, CallID: "c", Content: "recent result"},
		{Type: llm.TypeRuntimeNotification, Content: "A background job completed."},
		{Type: llm.TypeAssistant, Content: "Latest reasoning", Calls: []llm.ToolCall{{ID: "d"}}},
		{Type: llm.TypeToolResult, CallID: "d", Content: "latest result"},
	}
	for _, minimum := range []int{0, 200} {
		retention, err := Retain(messages, minimum, 700)
		if err != nil || retention.Start != 4 || !reflect.DeepEqual(retention.Inputs, []int{0}) {
			t.Fatalf("minimum %d: %+v, %v", minimum, retention, err)
		}
	}
	if _, err := Retain(messages[:len(messages)-1], 0, 200); err == nil {
		t.Fatal("unresolved call was discarded")
	}
	if _, err := Retain([]llm.Message{{Type: llm.TypeUserInput, Content: "Only a prompt."}}, 0, 200); err == nil {
		t.Fatal("compaction without model work reported progress")
	}
}
func TestRetentionCompactsEarlierHistoryWhenNewestTurnHasNoOlderCycle(t *testing.T) {
	for _, cycles := range []int{0, 1, 2} {
		for _, summary := range []bool{false, true} {
			prefix := []llm.Message{{Type: llm.TypeAssistant, Content: "Continuation summary"}}
			if !summary {
				prefix = []llm.Message{{Type: llm.TypeUserInput, Content: "Earlier task"}, {Type: llm.TypeAssistant, Content: "Earlier result"}}
			}
			messages := append(prefix, llm.Message{Type: llm.TypeUserInput, Content: strings.Repeat("new prompt ", 50)})
			for i := range cycles {
				id := string(rune('a' + i))
				messages = append(messages,
					llm.Message{Type: llm.TypeRuntimeContext, Content: "Live metadata"},
					llm.Message{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: id}}},
					llm.Message{Type: llm.TypeToolResult, CallID: id, Content: strings.Repeat("recent result ", 50)})
			}
			wantStart := len(messages)
			if cycles > 0 {
				wantStart = len(prefix) + 2 // User and the pre-cycle runtime message are not part of the tail.
			}
			retention, err := Retain(messages, 0, 700)
			wantInputs := []int{len(prefix)}
			if !summary {
				wantInputs = []int{0, len(prefix)}
			}
			if err != nil || retention.Start != wantStart || !reflect.DeepEqual(retention.Inputs, wantInputs) {
				t.Fatalf("cycles=%d summary=%v: %+v, %v", cycles, summary, retention, err)
			}
			if _, err := Retain(messages[len(prefix):], 0, 700); err == nil {
				t.Fatalf("cycles=%d: compaction reported progress without older history", cycles)
			}
			if cycles > 0 {
				if _, err := Retain(messages[:len(messages)-1], 0, 700); err == nil {
					t.Fatal("earlier history masked unresolved newest call")
				}
			}
		}
	}
}

func TestRetentionClampsTokenTargetAtCompleteCycleBoundaries(t *testing.T) {
	messages := []llm.Message{{Type: llm.TypeUserInput, Content: "Keep researching."}}
	starts := []int{}
	for i := range 5 {
		id := string(rune('a' + i))
		starts = append(starts, len(messages))
		messages = append(messages,
			llm.Message{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: id}}},
			llm.Message{Type: llm.TypeToolResult, CallID: id, Content: strings.Repeat("result ", 40)})
	}
	last := Tokens(messages[starts[4]:])
	for _, test := range []struct {
		name     string
		min, max int
		want     int
	}{
		{"two cycles", 0, 10 * last, starts[3]},
		{"soft minimum extends to three", 3 * last, 10 * last, starts[2]},
		{"hard maximum allows one", 0, last, starts[4]},
		{"boundary rounds down", 0, last + last/2, starts[4]},
		{"oversized fresh cycle protected", 0, last - 1, starts[4]},
	} {
		t.Run(test.name, func(t *testing.T) {
			retention, err := Retain(messages, test.min, test.max)
			if err != nil || retention.Start != test.want || !reflect.DeepEqual(retention.Inputs, []int{0}) {
				t.Fatal(retention, err)
			}
			if Tokens(messages[retention.Start:]) > test.max && retention.Start != starts[4] {
				t.Fatal("retained tail exceeded hard maximum")
			}
		})
	}
	for _, bounds := range [][2]int{{-1, 100}, {0, 0}, {101, 100}} {
		if _, err := Retain(messages, bounds[0], bounds[1]); err == nil {
			t.Fatal("accepted invalid bounds", bounds)
		}
	}
}

func TestRetentionPreservesUnconsumedBinaryCycle(t *testing.T) {
	messages := []llm.Message{
		{Type: llm.TypeUserInput, Content: "Earlier task"},
		{Type: llm.TypeAssistant, Content: "Earlier observations"},
		{Type: llm.TypeUserInput, Content: "Read a document"},
		{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: "document"}}},
		{Type: llm.TypeToolResult, CallID: "document", Files: []llm.BinaryFile{{Path: "/document.pdf", MIMEType: "application/pdf", Bytes: 20 << 10}}},
	}
	for _, trailing := range [][]llm.Message{nil, {{Type: llm.TypeUserInput, Content: "Continue"}}, {{Type: llm.TypeRuntimeContext, Content: "Live state"}}} {
		input := append(append([]llm.Message(nil), messages...), trailing...)
		retained, err := Retain(input, 0, 100)
		if err != nil || retained.Start != 3 {
			t.Fatal("unconsumed attachment was summarized", retained, err)
		}
		for _, i := range retained.Inputs {
			if i >= retained.Start {
				t.Fatal("protected suffix duplicates retained human input", retained)
			}
		}
	}
	consumed := append(messages, llm.Message{Type: llm.TypeAssistant, Content: "Document inspected"})
	retained, err := Retain(consumed, 0, 100)
	if err != nil || retained.Start <= 3 {
		t.Fatal("consumed attachment remained pinned", retained, err)
	}
	// With nothing older to summarize, fail instead of dropping the unread file.
	if _, err := Retain(messages[2:], 0, 100); err == nil {
		t.Fatal("unconsumed attachment discarded to manufacture compaction progress")
	}
	// Overlapping calls require retaining the earlier safe boundary, not just
	// the latest assistant message inside that still-open cycle.
	overlapping := append(append([]llm.Message(nil), messages[:4]...),
		llm.Message{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: "extra"}}},
		messages[4],
		llm.Message{Type: llm.TypeToolResult, CallID: "extra", Content: "Extra result"})
	retained, err = Retain(overlapping, 0, 100)
	if err != nil || retained.Start != 3 {
		t.Fatal("overlapping tool calls lost the unread original or its complete cycle", retained, err)
	}
}

func TestRetentionPreservesUnconsumedTextBatch(t *testing.T) {
	messages := []llm.Message{
		{Type: llm.TypeUserInput, Content: "Task", InputSource: "task"},
		{Type: llm.TypeAssistant, Content: strings.Repeat("older work ", 1000)},
		{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: "a"}, {ID: "b"}}},
		{Type: llm.TypeToolResult, CallID: "b", Content: strings.Repeat("fresh b ", 1000)},
		{Type: llm.TypeUserInput, InputSource: "steer", Content: "Keep going"},
		{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: "c"}}},
		{Type: llm.TypeToolResult, CallID: "c", Content: "fresh c"},
		{Type: llm.TypeToolResult, CallID: "a", Content: strings.Repeat("fresh a ", 1000)},
		{Type: llm.TypeRuntimeNotification, Content: "Timer fired"},
	}
	retained, err := Retain(messages, 0, 100)
	if err != nil || retained.Start != 2 || !reflect.DeepEqual(retained.Inputs, []int{0}) {
		t.Fatal("fresh text batch lost a pair or duplicated its steer", retained, err)
	}
	if _, err := Retain(append(messages[:1:1], messages[2:]...), 0, 100); !errors.Is(err, ErrNoCompactableHistory) {
		t.Fatal("no older work must fail without discarding fresh results", err)
	}
	consumed := append(append([]llm.Message(nil), messages...), llm.Message{Type: llm.TypeAssistant, Content: "All results inspected"})
	retained, err = Retain(consumed, 0, 100)
	if err != nil || retained.Start != len(consumed)-1 {
		t.Fatal("consumed oversized cycle did not become summarizable", retained, err)
	}
}

func TestRetentionSelectsCappedInputsInChronologicalOrder(t *testing.T) {
	messages := []llm.Message{
		{Type: llm.TypeUserInput, Content: "old normal"},
		{Type: llm.TypeAssistant, Content: "old work"},
		{Type: llm.TypeUserInput, InputSource: "steer", Content: "old steer"},
		{Type: llm.TypeUserInput, InputSource: "normal", Content: "normal"},
		{Type: llm.TypeUserInput, InputSource: "steer", Content: "first retained steer"},
		{Type: llm.TypeUserInput, InputSource: "queue", Content: "first retained ordinary"},
		{Type: llm.TypeRuntimeNotification, InputSource: "steer", Content: "not a steer"},
		{Type: llm.TypeUserInput, InputSource: "steer", Content: "second retained steer"},
		{Type: llm.TypeUserInput, Content: "second retained ordinary"},
		{Type: llm.TypeRuntimeContext, Content: "live state"},
		{Type: llm.TypeAssistant, Content: "recent work"},
	}
	retention, err := Retain(messages, 0, 1000)
	if err != nil || retention.Start != 10 || !reflect.DeepEqual(retention.Inputs, []int{4, 5, 7, 8}) {
		t.Fatal(retention, err)
	}
	for _, index := range retention.Inputs {
		if index >= retention.Start {
			t.Fatal("selected input overlaps model tail", retention)
		}
	}
	for _, source := range []string{"task", "btw"} {
		messages[8].InputSource = source
		retention, err := Retain(messages, 0, 1000)
		if err != nil || !reflect.DeepEqual(retention.Inputs, []int{4, 5, 7, 8}) {
			t.Fatal("task/aside did not count with ordinary prompts", retention, err)
		}
	}
	messages[8].InputSource = "unknown"
	if _, err := Retain(messages, 0, 1000); err == nil {
		t.Fatal("accepted unknown human input source")
	}
}

func TestRetentionRequiresUnretainedHistoryForProgress(t *testing.T) {
	messages := []llm.Message{
		{Type: llm.TypeRuntimeContext, Content: "retained input metadata"},
		{Type: llm.TypeUserInput, Content: "first ordinary"},
		{Type: llm.TypeUserInput, InputSource: "steer", Content: "first steer"},
		{Type: llm.TypeUserInput, InputSource: "queue", Content: "second ordinary"},
		{Type: llm.TypeRuntimeNotification, Content: "job completed"},
		{Type: llm.TypeUserInput, InputSource: "steer", Content: "second steer"},
		{Type: llm.TypeAssistant, Content: "recent model work"},
	}
	for _, end := range []int{len(messages) - 1, len(messages)} {
		if _, err := Retain(messages[:end], 0, 1000); err == nil {
			t.Fatal("selected inputs/runtime alone were treated as summary progress")
		}
	}
	messages = append([]llm.Message{{Type: llm.TypeUserInput, Content: "unselected old ordinary"}}, messages...)
	retention, err := Retain(messages, 0, 1000)
	if err != nil || retention.Start != 7 || !reflect.DeepEqual(retention.Inputs, []int{2, 3, 4, 6}) {
		t.Fatal("unselected human input was not valid summary progress", retention, err)
	}
}

func TestRetainInputSourceAndOriginalAge(t *testing.T) {
	at := time.UnixMilli(123456789)
	for _, source := range []string{"", "normal", "queue", "steer", "task", "btw"} {
		message := llm.Message{Type: llm.TypeUserInput, Content: "unchanged", InputSource: source, InputTimeMS: at.UnixMilli() - 98765}
		retained, err := RetainedInputMessages([]llm.Message{message}, []int{0}, at)
		if err != nil || len(retained) != 1 || retained[0].RetainedAtMS != at.UnixMilli() {
			t.Fatal(retained, err)
		}
		projected, err := ProjectRetainedInputs(retained)
		if err != nil || len(projected) != 2 || projected[0].Role() != "developer" || projected[0].Content != prompts.RetainedInputs || projected[1].Role() != "user" || !strings.Contains(projected[1].Content, `"timestamp":"98.77 seconds before compaction"`) {
			t.Fatal(projected, err)
		}
		later, err := RetainedInputMessages(retained, []int{0}, at.Add(time.Second))
		if err != nil || later[0].RetainedAtMS-later[0].InputTimeMS != 99765 {
			t.Fatal(later, err)
		}
	}
}

func TestRetainInputRejectsInvalidMetadata(t *testing.T) {
	at := time.UnixMilli(1000)
	valid := llm.Message{Type: llm.TypeUserInput, InputTimeMS: 500}
	for _, test := range []struct {
		name    string
		message llm.Message
		at      time.Time
	}{
		{"missing commit", llm.Message{Type: llm.TypeUserInput}, at},
		{"negative commit", llm.Message{Type: llm.TypeUserInput, InputTimeMS: -1}, at},
		{"future commit", llm.Message{Type: llm.TypeUserInput, InputTimeMS: 1001}, at},
		{"invalid source", llm.Message{Type: llm.TypeUserInput, InputSource: "invalid", InputTimeMS: 500}, at},
		{"runtime notice", llm.Message{Type: llm.TypeRuntimeNotification, InputTimeMS: 500}, at},
		{"assistant", llm.Message{Type: llm.TypeAssistant, InputTimeMS: 500}, at},
		{"zero compaction time", valid, time.Time{}},
		{"epoch compaction time", valid, time.UnixMilli(0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := RetainedInputMessages([]llm.Message{test.message}, []int{0}, test.at); err == nil {
				t.Fatal("accepted invalid retained input metadata")
			}
		})
	}
	valid.InputTimeMS = at.UnixMilli()
	if retained, err := RetainedInputMessages([]llm.Message{valid}, []int{0}, at); err != nil || retained[0].RetainedAtMS != valid.InputTimeMS {
		t.Fatal("equal commit/compaction timestamp rejected", retained, err)
	}
}

func TestRetainedInputMessagesPreserveExactOriginals(t *testing.T) {
	input := Input{Text: "original\n\nuser text", Source: "queue", Attachments: []Attachment{
		{Path: "notes.txt", Kind: "text", Text: "snapshot\ntext", Truncated: true},
		{Path: "/snapshot/image.png", Kind: "image", File: &llm.BinaryFile{Path: "/snapshot/image.png", SHA256: strings.Repeat("a", 64), MIMEType: "image/png", Bytes: 8}},
	}}
	original := input.Message()
	original.InputTimeMS = 500
	messages := []llm.Message{{Type: llm.TypeAssistant, Content: "not retained"}, original, {Type: llm.TypeUserInput, Content: "steer", InputSource: "steer", InputTimeMS: 700}}
	before, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := RetainedInputMessages(messages, []int{1, 2}, time.UnixMilli(1000))
	if err != nil || len(retained) != 2 {
		t.Fatal(retained, err)
	}
	wantOriginal, wantSteer := original, messages[2]
	wantOriginal.RetainedAtMS, wantSteer.RetainedAtMS = 1000, 1000
	if !reflect.DeepEqual(retained, []llm.Message{wantOriginal, wantSteer}) {
		t.Fatal("retained input changed or reordered", retained)
	}
	projected, err := ProjectRetainedInputs(retained)
	if err != nil {
		t.Fatal(err)
	}
	var group struct {
		Type   string `json:"type"`
		Inputs []struct {
			Content           string `json:"content"`
			AttachmentIndices []int  `json:"attachment_indices"`
		} `json:"inputs"`
	}
	if len(projected) != 2 || projected[0].Content != prompts.RetainedInputs || json.Unmarshal([]byte(projected[1].Content), &group) != nil || group.Type != "retained_inputs" || len(group.Inputs) != 2 || group.Inputs[0].Content != original.Content || group.Inputs[1].Content != "steer" || !reflect.DeepEqual(group.Inputs[0].AttachmentIndices, []int{0}) || !reflect.DeepEqual(projected[1].Files, original.Files) || projected[1].State != nil {
		t.Fatal("grouped projection lost text or attachment provenance", projected, group)
	}
	if Tokens(retained) != Tokens(projected) {
		t.Fatal("canonical and model projection budgets differ")
	}
	after, err := json.Marshal(messages)
	if err != nil || string(before) != string(after) {
		t.Fatal("retention mutated original messages", err)
	}
	for _, indices := range [][]int{{-1}, {3}, {2, 1}, {1, 1}, {0}, {0, 1, 2, 3, 4}} {
		if _, err := RetainedInputMessages(messages, indices, time.UnixMilli(1000)); err == nil {
			t.Fatal("invalid selection accepted", indices)
		}
	}
}

func TestInputMessagePreservesSourceAndJSONMetadata(t *testing.T) {
	for _, source := range []string{"", "normal", "queue", "steer", "task", "btw"} {
		for _, attachments := range [][]Attachment{nil, {{Kind: "text", Path: "x", Text: "snapshot"}}} {
			message := (Input{Text: "authored", Source: source, Attachments: attachments}).Message()
			if message.InputSource != source || message.DisplayText() != "authored" {
				t.Fatal("source or authored text changed", message)
			}
			message.InputTimeMS = 1000
			encoded, err := json.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			var decoded llm.Message
			if err := json.Unmarshal(encoded, &decoded); err != nil || !reflect.DeepEqual(message, decoded) {
				t.Fatal("input metadata JSON round trip changed message", decoded, err)
			}
			if source == "" && strings.Contains(string(encoded), "input_source") {
				t.Fatal("empty source was not omitted")
			}
			if !strings.Contains(string(encoded), `"input_time_ms":1000`) {
				t.Fatal("commit timestamp missing", string(encoded))
			}
		}
	}
}

func TestAttachmentsSnapshotAndDirectoryNoSymlinkTraversal(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "x.txt")
	os.WriteFile(p, []byte("before"), 0600)
	a, e := Snapshot(stdcontext.Background(), p, nil)
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(p, []byte("after"), 0600)
	if a.Text != "before" {
		t.Fatal("mutable snapshot")
	}
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0600)
	os.Symlink(outside, filepath.Join(root, "link"))
	dir, e := Snapshot(stdcontext.Background(), root, nil)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(dir.Text, "secret") {
		t.Fatal("followed symlink")
	}
	os.WriteFile(filepath.Join(root, "image.png"), []byte("\x89PNG\r\n\x1a\n"), 0600)
	if _, e = Snapshot(stdcontext.Background(), filepath.Join(root, "image.png"), nil); e == nil {
		t.Fatal("silently accepted unsupported image")
	}
}

func TestNativeReplayIsCountedOnce(t *testing.T) {
	native := []byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"result"}]}`)
	m := llm.Message{Type: llm.TypeAssistant, Content: "result", Calls: []llm.ToolCall{{ID: "c", Name: "read", Arguments: []byte(`{}`)}}, State: &llm.ReplayState{Provider: "openai", Model: "test", Version: 1, Items: []json.RawMessage{native}}}
	if got := Tokens([]llm.Message{m}); got != Estimate(string(native)) {
		t.Fatalf("native payload counted twice: %d", got)
	}
}

func TestNotificationTokensIncludeSemanticDeliveryMarker(t *testing.T) {
	m := llm.Message{Type: llm.TypeRuntimeNotification, Content: `{"type":"job_exit","job_id":"j"}`}
	content, err := m.ModelContent()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := Tokens([]llm.Message{m}), 16+Estimate(content); got != want || got <= 16+Estimate(m.Content) {
		t.Fatal("notification marker omitted from token occupancy", got, want)
	}
	if got := Tokens([]llm.Message{{Type: "unknown"}}); got != -1 {
		t.Fatal("invalid semantic type acquired valid occupancy", got)
	}
}

func TestAttachmentDisplayKeepsAuthoredTextAndExactModelInput(t *testing.T) {
	for _, text := range []string{"Inspect the attached file.", "", "Attachment (text): this is authored text"} {
		m := (Input{Text: text, Attachments: []Attachment{{Kind: "text", Path: "notes.txt", Text: "immutable snapshot", Truncated: true}, {Path: "/snapshot/field.png", Kind: "image", File: &llm.BinaryFile{Path: "/snapshot/field.png", SHA256: strings.Repeat("b", 64), MIMEType: "image/png", Bytes: 8}}}}).Message()
		if m.DisplayText() != text || !strings.Contains(m.Content, "immutable snapshot") || !strings.Contains(m.Content, "[attachment truncated]") || len(m.Files) != 1 {
			t.Fatal("presentation lost authored text or model attachments", m)
		}
		encoded, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var reloaded llm.Message
		if err := json.Unmarshal(encoded, &reloaded); err != nil || reloaded.DisplayText() != text || reloaded.Content != m.Content {
			t.Fatal("round trip changed attachment input", reloaded, err)
		}
		m.Type = llm.TypeRuntimeNotification
		if m.DisplayText() != m.Content {
			t.Fatal("runtime notice was hidden as user input")
		}
	}
	if m := (Input{Text: "Ordinary input"}).Message(); m.UserText != nil || m.DisplayText() != m.Content {
		t.Fatal("ordinary input acquired unnecessary display metadata", m)
	}
}

func TestInputMessageManyAttachments(t *testing.T) {
	input := Input{Text: "original", Attachments: make([]Attachment, 100)}
	for i := range input.Attachments {
		input.Attachments[i] = Attachment{Kind: "text", Path: "notes.txt", Text: strings.Repeat("λ", 1024), Truncated: true}
	}
	want := input.Text + strings.Repeat("\n\nAttachment (text): notes.txt\n"+input.Attachments[0].Text+"\n[attachment truncated]", 100)
	message := input.Message()
	if message.Content != want || message.DisplayText() != input.Text {
		t.Fatal("attachment construction changed exact source or presentation")
	}
}

func BenchmarkInputMessageAttachments(b *testing.B) {
	input := Input{Text: "original", Attachments: make([]Attachment, 100)}
	for i := range input.Attachments {
		input.Attachments[i] = Attachment{Kind: "text", Path: "notes.txt", Text: strings.Repeat("a", 32<<10)}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = input.Message()
	}
}
