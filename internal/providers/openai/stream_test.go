package openai

import (
	"encoding/json"
	"strings"
	"testing"
	"ttc/internal/llm"
	"unicode/utf8"
)

func TestStreamFailuresPreserveOfficialErrorFields(t *testing.T) {
	for _, test := range []struct {
		name  string
		event map[string]any
		want  []string
	}{
		{"failed", map[string]any{"type": "response.failed", "response": map[string]any{"error": map[string]string{"code": "context_length_exceeded", "message": "Reduce input or compact the conversation."}}}, []string{"context_length_exceeded", "Reduce input or compact"}},
		{"error", map[string]any{"type": "error", "code": "invalid_request_error", "message": "Remove unsupported reasoning effort.", "param": "reasoning.effort"}, []string{"invalid_request_error", "Remove unsupported", "parameter=reasoning.effort"}},
		{"incomplete", map[string]any{"type": "response.incomplete", "response": map[string]any{"incomplete_details": map[string]string{"reason": "max_output_tokens"}}}, []string{"reason=max_output_tokens"}},
		{"nullable", map[string]any{"type": "error", "code": nil, "message": "Retry with fewer tools.", "param": nil}, []string{"Retry with fewer tools"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// An announced call must remain unexecuted after a terminal failure.
			announcement := callEvents(0, "fc_pending", "pending", "read", `{}`)[0]
			calls := 0
			committed, err := parseStream(strings.NewReader(sseFrames(announcement, test.event)), func(event llm.StreamEvent) error {
				if event.Kind == "call" {
					calls++
				}
				return nil
			})
			if !committed || err == nil || calls != 0 {
				t.Fatal("terminal failure executed or lost a pending call", committed, calls, err)
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("lost recovery detail %q: %v", want, err)
				}
			}
		})
	}
}

func TestStreamFailureDiagnosticsAreBoundedAndControlSafe(t *testing.T) {
	event := map[string]any{"type": "error", "code": "invalid_request_error", "message": "Adjust the request.\n\t\x1b\u202e" + strings.Repeat("研究", 3000)}
	_, err := parseStream(strings.NewReader(sseFrames(event)), func(llm.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("error event accepted")
	}
	text := err.Error()
	if len(text) > 4096+64 || !utf8.ValidString(text) || !strings.HasSuffix(text, "…") || strings.ContainsAny(text, "\x1b\u202e\n\t") || !strings.Contains(text, "Adjust the request.") {
		t.Fatalf("unsafe or unbounded provider diagnostic: %q", text)
	}
}

func sseFrames(events ...any) string {
	var text strings.Builder
	for _, event := range events {
		b, err := json.Marshal(event)
		if err != nil {
			panic(err)
		}
		text.WriteString("data: ")
		text.Write(b)
		text.WriteString("\n\n")
	}
	return text.String()
}

func TestRejectsDuplicateItemIDsAcrossOutputIndices(t *testing.T) {
	a, b := callEvents(1, "same", "call_a", "read", `{}`), callEvents(2, "same", "call_b", "read", `{}`)
	stream := sseFrames(a[0], b[0], map[string]any{"type": "response.completed"})
	calls := 0
	_, err := parseStream(strings.NewReader(stream), func(ev llm.StreamEvent) error {
		if ev.Kind == "call" {
			calls++
		}
		return nil
	})
	if err == nil || calls != 0 {
		t.Fatal("duplicate item identity accepted", err, calls)
	}
}

func TestToolCallsPreserveOutputOrderWithReverseCompletion(t *testing.T) {
	a, b := callEvents(2, "fc_a", "call_a", "write", `{"path":"x","content":"first"}`), callEvents(4, "fc_b", "call_b", "write", `{"path":"x","content":"second"}`)
	var stream strings.Builder
	stream.WriteString(sseFrames(a[0]))
	for _, event := range b {
		stream.WriteString(sseFrames(event))
	}
	for _, event := range a[1:] {
		stream.WriteString(sseFrames(event))
	}
	stream.WriteString(sseFrames(map[string]any{"type": "response.completed"}))
	var calls []llm.ToolCall
	_, err := parseStream(strings.NewReader(stream.String()), func(ev llm.StreamEvent) error {
		if ev.Kind == "call" {
			calls = append(calls, *ev.Call)
		}
		return nil
	})
	if err != nil || len(calls) != 2 || calls[0].ID != "call_a" || calls[1].ID != "call_b" {
		t.Fatal("completion order reversed file mutations", calls, err)
	}
}
func callEvents(index int, itemID, id, name, arguments string) []map[string]any {
	item := map[string]any{"type": "function_call", "id": itemID, "call_id": id, "name": name, "arguments": ""}
	runes := []rune(arguments)
	split := len(runes) / 2
	return []map[string]any{
		{"type": "response.output_item.added", "output_index": index, "item": item},
		{"type": "response.function_call_arguments.delta", "output_index": index, "item_id": itemID, "delta": string(runes[:split])},
		{"type": "response.function_call_arguments.delta", "output_index": index, "item_id": itemID, "delta": string(runes[split:])},
		{"type": "response.function_call_arguments.done", "output_index": index, "item_id": itemID, "arguments": arguments},
		{"type": "response.output_item.done", "output_index": index, "item": map[string]any{"type": "function_call", "id": itemID, "call_id": id, "name": name, "arguments": arguments}},
	}
}
func functionFrames(index int, itemID, id, name, arguments string) string {
	var b strings.Builder
	for _, event := range callEvents(index, itemID, id, name, arguments) {
		b.WriteString(sseFrames(event))
	}
	return b.String()
}

func TestInterleavedToolArgumentAssembly(t *testing.T) {
	args := []string{`{"path":"研究.txt","content":"line\n\"quote\" \ud83d\ude80"}`, `{"command":"printf 'hello'"}`}
	a, b := callEvents(2, "fc_a", "call_a", "write", args[0]), callEvents(4, "fc_b", "call_b", "shell", args[1])
	var stream strings.Builder
	for i := range a {
		stream.WriteString(sseFrames(a[i], b[i]))
	}
	stream.WriteString(sseFrames(map[string]any{"type": "response.completed"}))
	var calls []llm.ToolCall
	progress := map[string]llm.ToolProgress{}
	starts := 0
	committed, err := parseStream(strings.NewReader(stream.String()), func(ev llm.StreamEvent) error {
		if ev.Kind == "call_start" {
			starts++
			if ev.Call != nil || ev.CallStart.Name == "" {
				t.Fatal("announcement contains executable call")
			}
		}
		if ev.Kind == "call" {
			calls = append(calls, *ev.Call)
		}
		if ev.Kind == "call_progress" {
			progress[ev.CallProgress.ID] = *ev.CallProgress
		}
		return nil
	})
	if err != nil || !committed || starts != 2 || len(calls) != 2 {
		t.Fatal(committed, err, starts, calls)
	}
	for i, c := range calls {
		if string(c.Arguments) != args[i] {
			t.Fatal("argument fragment lost or duplicated", c)
		}
		if got := progress[c.ID]; got.Segments != 2 || got.Bytes != len(args[i]) {
			t.Fatal("argument progress count was not per call or byte exact", c.ID, got)
		}
	}
}

func TestMalformedToolStreamNeverEmitsExecutableCall(t *testing.T) {
	for _, name := range []string{"unknown_item", "wrong_index", "changed_identity", "conflicting_final", "duplicate_done", "missing_done", "invalid_json", "unfinished", "late_delta"} {
		t.Run(name, func(t *testing.T) {
			events := callEvents(0, "fc", "call", "write", `{"path":"x"}`)
			switch name {
			case "unknown_item":
				events[1]["item_id"] = "other"
			case "wrong_index":
				events[1]["output_index"] = 1
			case "changed_identity":
				events[4]["item"].(map[string]any)["call_id"] = "other"
			case "conflicting_final":
				events[4]["item"].(map[string]any)["arguments"] = `{"path":"other"}`
			case "duplicate_done":
				events = append(events[:4], append([]map[string]any{events[3]}, events[4:]...)...)
			case "missing_done":
				events = append(events[:3], events[4])
			case "invalid_json":
				events = callEvents(0, "fc", "call", "write", `{"path":`)
			case "unfinished":
				events = events[:2]
			case "late_delta":
				events = append(events[:4], events[1], events[4])
			}
			var stream strings.Builder
			for _, event := range events {
				stream.WriteString(sseFrames(event))
			}
			stream.WriteString(sseFrames(map[string]any{"type": "response.completed"}))
			starts, calls := 0, 0
			_, err := parseStream(strings.NewReader(stream.String()), func(ev llm.StreamEvent) error {
				if ev.Kind == "call_start" {
					starts++
				}
				if ev.Kind == "call" {
					calls++
				}
				return nil
			})
			if err == nil || calls != 0 || starts != 1 {
				t.Fatal(err, starts, calls)
			}
		})
	}
}

func TestFinalizedToolArgumentsOverrideDeltas(t *testing.T) {
	events := callEvents(0, "fc", "call", "read", `{"path":"original"}`)
	events[3]["arguments"] = `{"path":"final"}`
	events[4]["item"].(map[string]any)["arguments"] = `{"path":"final"}`
	var calls []llm.ToolCall
	_, err := parseStream(strings.NewReader(sseFrames(events[0], events[1], events[2], events[3], events[4], map[string]any{"type": "response.completed"})), func(ev llm.StreamEvent) error {
		if ev.Kind == "call" {
			calls = append(calls, *ev.Call)
		}
		return nil
	})
	if err != nil || len(calls) != 1 || string(calls[0].Arguments) != `{"path":"final"}` {
		t.Fatal("finalized arguments were not used", err, calls)
	}
}

func TestArgumentsAccumulateBeforeExecutionAndTruncation(t *testing.T) {
	events := callEvents(0, "fc", "call", "read", `{"path":"x"}`)
	// An argument snapshot alone is not a completed output item.
	var stream strings.Builder
	for _, event := range events[:4] {
		stream.WriteString(sseFrames(event))
	}
	starts, calls := 0, 0
	committed, err := parseStream(strings.NewReader(stream.String()), func(ev llm.StreamEvent) error {
		if ev.Kind == "call_start" {
			starts++
		}
		if ev.Kind == "call" {
			calls++
		}
		return nil
	})
	if !committed || err != errStreamLost || starts != 1 || calls != 0 {
		t.Fatal(committed, err, starts, calls)
	}
}
