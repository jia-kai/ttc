package context

import (
	stdcontext "context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"scicode/internal/prompts"
	"scicode/internal/provider"
	"strings"
	"testing"
	"time"
)

func TestRecentCycleRetentionAndBudgets(t *testing.T) {
	m := []provider.Message{{Role: "user", Content: strings.Repeat("a", 600)}, {Role: "assistant", Content: "old"}, {Role: "user", Content: "new"}, {Role: "assistant", Calls: []provider.ToolCall{{ID: "c", Name: "read", Arguments: []byte(`{"path":"x"}`)}}}, {Role: "tool", CallID: "c", Content: `{"ok":true}`}}
	cut, e := Retain(m, 0, 200)
	if e != nil || cut.Start != 3 || !reflect.DeepEqual(cut.Inputs, []int{0, 2}) {
		t.Fatalf("cut=%+v err=%v", cut, e)
	}
	sel := provider.Selection{Model: provider.ScriptModel()}
	if !Fits(sel, "instructions", nil, m, false) {
		t.Fatal("small request did not fit")
	}
	if Fits(sel, strings.Repeat("a", 100000), nil, m, false) {
		t.Fatal("oversize request fit")
	}
}

func TestRetentionKeepsFittingRecentCyclesAndBalancedParallelTools(t *testing.T) {
	messages := []provider.Message{
		{Role: "user", Content: "Keep researching."},
		{Role: "assistant", Content: strings.Repeat("old ", 1000), Calls: []provider.ToolCall{{ID: "a"}, {ID: "b"}}},
		{Role: "tool", CallID: "b", Content: "second result"},
		{Role: "tool", CallID: "a", Content: "first result"},
		{Role: "assistant", Content: "Recent reasoning", Calls: []provider.ToolCall{{ID: "c"}}},
		{Role: "tool", CallID: "c", Content: "recent result"},
		{Role: "user", Runtime: true, Content: "A background job completed."},
		{Role: "assistant", Content: "Latest reasoning", Calls: []provider.ToolCall{{ID: "d"}}},
		{Role: "tool", CallID: "d", Content: "latest result"},
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
	if _, err := Retain([]provider.Message{{Role: "user", Content: "Only a prompt."}}, 0, 200); err == nil {
		t.Fatal("compaction without model work reported progress")
	}
}
func TestRetentionCompactsEarlierHistoryWhenNewestTurnHasNoOlderCycle(t *testing.T) {
	for _, cycles := range []int{0, 1, 2} {
		for _, summary := range []bool{false, true} {
			prefix := []provider.Message{{Role: "assistant", Content: "Continuation summary"}}
			if !summary {
				prefix = []provider.Message{{Role: "user", Content: "Earlier task"}, {Role: "assistant", Content: "Earlier result"}}
			}
			messages := append(prefix, provider.Message{Role: "user", Content: strings.Repeat("new prompt ", 50)})
			for i := range cycles {
				id := string(rune('a' + i))
				messages = append(messages,
					provider.Message{Role: "developer", Runtime: true, Content: "Live metadata"},
					provider.Message{Role: "assistant", Calls: []provider.ToolCall{{ID: id}}},
					provider.Message{Role: "tool", CallID: id, Content: strings.Repeat("recent result ", 50)})
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
	messages := []provider.Message{{Role: "user", Content: "Keep researching."}}
	starts := []int{}
	for i := range 5 {
		id := string(rune('a' + i))
		starts = append(starts, len(messages))
		messages = append(messages,
			provider.Message{Role: "assistant", Calls: []provider.ToolCall{{ID: id}}},
			provider.Message{Role: "tool", CallID: id, Content: strings.Repeat("result ", 40)})
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
		{"oversized latest cycle summarized", 0, last - 1, len(messages)},
	} {
		t.Run(test.name, func(t *testing.T) {
			retention, err := Retain(messages, test.min, test.max)
			if err != nil || retention.Start != test.want || !reflect.DeepEqual(retention.Inputs, []int{0}) {
				t.Fatal(retention, err)
			}
			if Tokens(messages[retention.Start:]) > test.max {
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

func TestRetentionSelectsCappedInputsInChronologicalOrder(t *testing.T) {
	messages := []provider.Message{
		{Role: "user", Content: "old normal"},
		{Role: "assistant", Content: "old work"},
		{Role: "user", InputSource: "steer", Content: "old steer"},
		{Role: "user", InputSource: "normal", Content: "normal"},
		{Role: "user", InputSource: "steer", Content: "first retained steer"},
		{Role: "user", InputSource: "queue", Content: "first retained ordinary"},
		{Role: "user", Runtime: true, InputSource: "steer", Content: "not a steer"},
		{Role: "user", InputSource: "steer", Content: "second retained steer"},
		{Role: "user", Content: "second retained ordinary"},
		{Role: "developer", Runtime: true, Content: "live state"},
		{Role: "assistant", Content: "recent work"},
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
	messages := []provider.Message{
		{Role: "developer", Runtime: true, Content: "retained input metadata"},
		{Role: "user", Content: "first ordinary"},
		{Role: "user", InputSource: "steer", Content: "first steer"},
		{Role: "user", InputSource: "queue", Content: "second ordinary"},
		{Role: "user", Runtime: true, Content: "job completed"},
		{Role: "user", InputSource: "steer", Content: "second steer"},
		{Role: "assistant", Content: "recent model work"},
	}
	for _, end := range []int{len(messages) - 1, len(messages)} {
		if _, err := Retain(messages[:end], 0, 1000); err == nil {
			t.Fatal("selected inputs/runtime alone were treated as summary progress")
		}
	}
	messages = append([]provider.Message{{Role: "user", Content: "unselected old ordinary"}}, messages...)
	retention, err := Retain(messages, 0, 1000)
	if err != nil || retention.Start != 7 || !reflect.DeepEqual(retention.Inputs, []int{2, 3, 4, 6}) {
		t.Fatal("unselected human input was not valid summary progress", retention, err)
	}
}

func TestInputMarkerSourceAndOriginalAge(t *testing.T) {
	at := time.UnixMilli(123456789)
	for _, source := range []string{"", "normal", "queue", "steer", "task", "btw"} {
		message := provider.Message{Role: "user", Content: "unchanged", InputSource: source, InputTimeMS: at.UnixMilli() - 98765}
		marker, err := InputMarker(message, at)
		if err != nil || marker.Role != "developer" || !marker.Runtime || !strings.HasPrefix(marker.Content, prompts.RetainedInput+"\n") {
			t.Fatal(marker, err)
		}
		var metadata struct {
			Type                  string `json:"type"`
			Source                string `json:"source"`
			OriginalCommittedAtMS int64  `json:"original_committed_at_ms"`
			CompactionAtMS        int64  `json:"compaction_at_ms"`
			AgeMS                 int64  `json:"age_ms"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(marker.Content, prompts.RetainedInput+"\n")), &metadata); err != nil {
			t.Fatal(err)
		}
		wantSource := source
		if wantSource == "" {
			wantSource = "normal"
		}
		if metadata.Type != "retained_input" || metadata.Source != wantSource || metadata.OriginalCommittedAtMS != message.InputTimeMS || metadata.CompactionAtMS != at.UnixMilli() || metadata.AgeMS != 98765 {
			t.Fatal(metadata)
		}
		// Recompaction must measure from the original commit, not the last marker.
		later, err := InputMarker(message, at.Add(time.Second))
		if err != nil || !strings.Contains(later.Content, `"age_ms":99765`) {
			t.Fatal(later, err)
		}
	}
}

func TestInputMarkerRejectsInvalidMetadata(t *testing.T) {
	at := time.UnixMilli(1000)
	valid := provider.Message{Role: "user", InputTimeMS: 500}
	for _, test := range []struct {
		name    string
		message provider.Message
		at      time.Time
	}{
		{"missing commit", provider.Message{Role: "user"}, at},
		{"negative commit", provider.Message{Role: "user", InputTimeMS: -1}, at},
		{"future commit", provider.Message{Role: "user", InputTimeMS: 1001}, at},
		{"invalid source", provider.Message{Role: "user", InputSource: "invalid", InputTimeMS: 500}, at},
		{"runtime notice", provider.Message{Role: "user", Runtime: true, InputTimeMS: 500}, at},
		{"assistant", provider.Message{Role: "assistant", InputTimeMS: 500}, at},
		{"zero compaction time", valid, time.Time{}},
		{"epoch compaction time", valid, time.UnixMilli(0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := InputMarker(test.message, test.at); err == nil {
				t.Fatal("accepted invalid retained input metadata")
			}
		})
	}
	valid.InputTimeMS = at.UnixMilli()
	if marker, err := InputMarker(valid, at); err != nil || !strings.Contains(marker.Content, `"age_ms":0`) {
		t.Fatal("equal commit/compaction timestamp rejected", marker, err)
	}
}

func TestRetainedInputMessagesPreserveExactOriginals(t *testing.T) {
	input := Input{Text: "original\n\nuser text", Source: "queue", Attachments: []Attachment{
		{Path: "notes.txt", Kind: "text", Text: "snapshot\ntext", Truncated: true},
		{Path: "image.png", Image: &provider.Image{Path: "image.png", DataURL: "data:image/png;base64,original"}},
	}}
	original := input.Message()
	original.InputTimeMS = 500
	original.State = &provider.ReplayState{Provider: "test", Model: "model", Version: 1, Items: []json.RawMessage{json.RawMessage(`{"original":true}`)}}
	messages := []provider.Message{{Role: "assistant", Content: "not retained"}, original, {Role: "user", Content: "steer", InputSource: "steer", InputTimeMS: 700}}
	before, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := RetainedInputMessages(messages, []int{1, 2}, time.UnixMilli(1000))
	if err != nil || len(retained) != 4 {
		t.Fatal(retained, err)
	}
	if !reflect.DeepEqual(retained[1], original) || !reflect.DeepEqual(retained[3], messages[2]) || retained[0].Role != "developer" || retained[2].Role != "developer" {
		t.Fatal("retained input changed or reordered", retained)
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
			var decoded provider.Message
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
	a, e := Snapshot(stdcontext.Background(), p, false)
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
	dir, e := Snapshot(stdcontext.Background(), root, false)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(dir.Text, "secret") {
		t.Fatal("followed symlink")
	}
	os.WriteFile(filepath.Join(root, "image.png"), []byte("image"), 0600)
	if _, e = Snapshot(stdcontext.Background(), filepath.Join(root, "image.png"), false); e == nil {
		t.Fatal("silently accepted unsupported image")
	}
}

func TestNativeReplayIsCountedOnce(t *testing.T) {
	native := []byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"result"}]}`)
	m := provider.Message{Role: "assistant", Content: "result", Calls: []provider.ToolCall{{ID: "c", Name: "read", Arguments: []byte(`{}`)}}, State: &provider.ReplayState{Provider: "openai", Model: "test", Version: 1, Items: []json.RawMessage{native}}}
	if got := Tokens([]provider.Message{m}); got != Estimate(string(native)) {
		t.Fatalf("native payload counted twice: %d", got)
	}
}

func TestAttachmentDisplayKeepsAuthoredTextAndExactModelInput(t *testing.T) {
	for _, text := range []string{"Inspect the attached file.", "", "Attachment (text): this is authored text"} {
		m := (Input{Text: text, Attachments: []Attachment{{Kind: "text", Path: "notes.txt", Text: "immutable snapshot", Truncated: true}, {Path: "field.png", Image: &provider.Image{Path: "field.png", DataURL: "data:image/png;base64,snapshot"}}}}).Message()
		if m.DisplayText() != text || !strings.Contains(m.Content, "immutable snapshot") || !strings.Contains(m.Content, "[attachment truncated]") || len(m.Images) != 1 {
			t.Fatal("presentation lost authored text or model attachments", m)
		}
		encoded, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var reloaded provider.Message
		if err := json.Unmarshal(encoded, &reloaded); err != nil || reloaded.DisplayText() != text || reloaded.Content != m.Content {
			t.Fatal("round trip changed attachment input", reloaded, err)
		}
		m.Runtime = true
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
