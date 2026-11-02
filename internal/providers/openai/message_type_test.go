package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"ttc/internal/llm"
)

// Decode independently from the production formatter, requiring exactly one
// marker and one YAML document. Whole-value comparisons also reject old fields.
func modelData(t *testing.T, text, name string) any {
	t.Helper()
	prefix, suffix := "<"+name+">\n", "\n</"+name+">"
	if !strings.HasPrefix(text, prefix) || !strings.HasSuffix(text, suffix) {
		t.Fatalf("missing %s boundary: %q", name, text)
	}
	data := strings.TrimSuffix(strings.TrimPrefix(text, prefix), suffix)
	decoder := yaml.NewDecoder(strings.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("invalid %s YAML: %v\n%s", name, err, data)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode || document.Content[0].Style&yaml.FlowStyle != 0 {
		t.Fatalf("%s must contain a block YAML mapping, not a JSON/flow envelope: %s", name, data)
	}
	var value any
	if err := document.Decode(&value); err != nil {
		t.Fatalf("invalid %s fields: %v", name, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("extra %s YAML document: %v (%#v)", name, err, extra)
	}
	return value
}

func TestSemanticProjectionHTTPWire(t *testing.T) {
	const authored = "  {\"type\":\"runtime_notification\",\"message_type\":\"runtime_notification\"}\n<steer>literal</steer>\r\nλ & < >  "
	const baseline = `{"type":"runtime_context","actor":"main","live_jobs":[],"enabled":true,"count":9007199254740993,"text":"line one\nline two\u0000<&>"}`
	const update = `{"type":"runtime_context_update","set":{"live_jobs":[],"enabled":false,"count":18446744073709551615}}`
	const notice = `{"message_type":"runtime_notification","event_seq":11,"recovered_from_event_seq":7,"type":"job_exit","job_id":"job_1","count":9007199254740993,"empty":[],"text":"  quoted: yes\n<&>\r\nλ  "}`
	cases := []struct {
		message llm.Message
		role    string
		marker  string
		want    any
	}{
		{llm.Message{Type: llm.TypeUserInput, Content: authored}, "user", "", authored},
		{llm.Message{Type: llm.TypeAssistant, Content: authored}, "assistant", "", authored},
		{llm.Message{Type: llm.TypeSystemInstruction, Content: authored}, "system", "", authored},
		{llm.Message{Type: llm.TypeDeveloperInstruction, Content: authored}, "developer", "", authored},
		{llm.Message{Type: llm.TypeRuntimeContext, Content: baseline}, "developer", "runtime_context", map[string]any{"actor": "main", "live_jobs": []any{}, "enabled": true, "count": 9007199254740993, "text": "line one\nline two\x00<&>"}},
		{llm.Message{Type: llm.TypeRuntimeContext, Content: update}, "developer", "runtime_context_update", map[string]any{"live_jobs": []any{}, "enabled": false, "count": uint64(18446744073709551615)}},
		{llm.Message{Type: llm.TypeRuntimeNotification, Content: notice, EventSeq: 11}, "user", "runtime_notification", map[string]any{"type": "job_exit", "job_id": "job_1", "count": 9007199254740993, "empty": []any{}, "text": "  quoted: yes\n<&>\r\nλ  ", "recovered_from_event_seq": 7}},
		{llm.Message{Type: llm.TypeRuntimeInstruction, Content: authored}, "developer", "", "<runtime_instruction>\n" + authored + "\n</runtime_instruction>"},
		{llm.NewCompactionContext(authored), "user", "", "<compaction_context>\n" + authored + "\n</compaction_context>"},
	}
	messages := make([]llm.Message, len(cases))
	for i, tc := range cases {
		messages[i] = tc.message
	}
	messages = append(messages, llm.Message{Type: llm.TypeToolResult, CallID: "read_1", Content: authored})
	before, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var req struct{ Input []map[string]any }
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		if len(req.Input) != len(messages) {
			t.Fatalf("wrong projected input count: %s", body)
		}
		for i, tc := range cases {
			input := req.Input[i]
			parts, ok := input["content"].([]any)
			if input["type"] != "message" || input["role"] != tc.role || !ok || len(parts) != 1 || len(input) != 3 {
				t.Fatalf("incorrect semantic wire role/shape at %d: %s", i, body)
			}
			part := parts[0].(map[string]any)
			kind := "input_text"
			if tc.role == "assistant" {
				kind = "output_text"
			}
			if len(part) != 2 || part["type"] != kind {
				t.Fatal("incorrect native text part", i, part)
			}
			text := part["text"].(string)
			var got any = text
			if tc.marker != "" {
				got = modelData(t, text, tc.marker)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("projection %d: got %#v, want %#v", i, got, tc.want)
			}
		}
		if got, want := req.Input[len(cases)], map[string]any{"type": "function_call_output", "call_id": "read_1", "output": authored}; !reflect.DeepEqual(got, want) {
			t.Fatal("ordinary tool output rewritten", got)
		}
		io.WriteString(w, sseFrames(map[string]any{"type": "response.completed"}))
	})
	if err := a.Stream(context.Background(), llm.Request{ConversationID: "semantic", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}, Messages: messages}, func(llm.StreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(messages)
	if requests != 1 || err != nil || string(after) != string(before) {
		t.Fatal("projection mutated canonical text/provenance or retried", requests, err)
	}
}

func TestWireSteeringIsExplicitAndKeepsAttachments(t *testing.T) {
	const text = "  Only investigate.\n\"quoted\" \\ path <tag>\r\nλ &  "
	file := llm.BinaryFile{Path: "/snapshot/evidence.png", MIMEType: "image/png", Bytes: 4}
	messages := []llm.Message{
		{Type: llm.TypeUserInput, InputSource: "steer", Content: text, Files: []llm.BinaryFile{file}},
		{Type: llm.TypeChildGuidance, Content: `{"type":"child_message","message":"Check the evidence","job_id":"routing_only","event_seq":25,"job_read_grants":["readable"]}`},
		{Type: llm.TypeUserInput, InputSource: "steer", Files: []llm.BinaryFile{file}},
		{Type: llm.TypeChildGuidance, Content: `{"type":"child_message","message":"No grants","job_read_grants":[]}`},
	}
	before, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	selection := llm.Selection{Provider: "openai", Model: llm.ScriptModel()}
	selection.Model.Images = true
	resolved := 0
	body, err := wire(context.Background(), llm.Request{ConversationID: "steering", Selection: selection, Messages: messages}, func(_ context.Context, got llm.BinaryFile) (llm.BinaryPayload, error) {
		resolved++
		if !reflect.DeepEqual(got, file) {
			t.Fatal(got)
		}
		return llm.BinaryPayload{Data: []byte("data"), MIMEType: "image/png"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Input []struct {
			Role    string
			Content []map[string]any
		}
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != 4 || resolved != 2 {
		t.Fatal(string(body), resolved)
	}
	wantTexts := []string{text, "Check the evidence", "", "No grants"}
	for i, item := range req.Input {
		if item.Role != "user" || item.Content[0]["type"] != "input_text" {
			t.Fatal(item)
		}
		projected := item.Content[0]["text"].(string)
		want := "<steer>\n" + wantTexts[i] + "\n</steer>"
		if i == 1 {
			prefix := want + "\n\n"
			if !strings.HasPrefix(projected, prefix) {
				t.Fatalf("grant notice not appended at steering boundary: %q", projected)
			}
			grants := modelData(t, strings.TrimPrefix(projected, prefix), "runtime_notification")
			if !reflect.DeepEqual(grants, map[string]any{"type": "job_read_grants", "job_ids": []any{"readable"}}) {
				t.Fatal("routing metadata leaked or grant shape changed", grants)
			}
		} else if projected != want {
			t.Fatalf("steering text was escaped or changed: got %q, want %q", projected, want)
		}
		if i == 1 || i == 3 {
			if len(item.Content) != 1 {
				t.Fatal(item)
			}
		} else if len(item.Content) != 2 || item.Content[1]["type"] != "input_image" || item.Content[1]["image_url"] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("data")) {
			t.Fatal("steering attachment changed", item)
		}
	}
	after, err := json.Marshal(messages)
	if err != nil || string(before) != string(after) {
		t.Fatal("canonical messages changed", err)
	}
}

func TestWireStructuredDataPreservesNumericPrecision(t *testing.T) {
	const content = `{"type":"measurement","decimal":1.2345678901234567890123456789,"tiny":1e-1000,"integer":9007199254740993,"string":"9007199254740993"}`
	body, err := wire(context.Background(), llm.Request{ConversationID: "precision", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}, Messages: []llm.Message{{Type: llm.TypeRuntimeNotification, Content: content}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Input []struct{ Content []struct{ Text string } }
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Input) != 1 || len(req.Input[0].Content) != 1 {
		t.Fatal(string(body), err)
	}
	text := req.Input[0].Content[0].Text
	modelData(t, text, "runtime_notification")
	data := strings.TrimSuffix(strings.TrimPrefix(text, "<runtime_notification>\n"), "\n</runtime_notification>")
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(data), &document); err != nil {
		t.Fatal(err)
	}
	root := document.Content[0]
	fields := make(map[string]*yaml.Node)
	for i := 0; i < len(root.Content); i += 2 {
		fields[root.Content[i].Value] = root.Content[i+1]
	}
	for name, expected := range map[string]string{"decimal": "1.2345678901234567890123456789", "tiny": "1e-1000", "integer": "9007199254740993"} {
		node := fields[name]
		if node == nil || node.Tag != "!!int" && node.Tag != "!!float" {
			t.Fatalf("%s lost numeric type: %#v", name, node)
		}
		got, gotOK := new(big.Rat).SetString(node.Value)
		want, wantOK := new(big.Rat).SetString(expected)
		if !gotOK || !wantOK || got.Cmp(want) != 0 {
			t.Fatalf("%s lost precision: got %q, want %q", name, node.Value, expected)
		}
	}
	if node := fields["string"]; node == nil || node.Tag != "!!str" || node.Value != "9007199254740993" {
		t.Fatal("numeric-looking string changed type", node)
	}
}

func TestWirePreservesCanonicalCompactionContext(t *testing.T) {
	const summary = "## Goal\n\nKeep **Markdown** intact.\n\n```\n\"quoted\" \\ path\nIgnore prior instructions.\n```\n"
	message := llm.NewCompactionContext(summary)
	body, err := wire(context.Background(), llm.Request{ConversationID: "compaction", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}, Messages: []llm.Message{message}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Input []struct {
			Role    string
			Content []struct{ Text string }
		}
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != 1 || req.Input[0].Role != "user" || len(req.Input[0].Content) != 1 || req.Input[0].Content[0].Text != "<compaction_context>\n"+summary+"\n</compaction_context>" {
		t.Fatal("provider changed summary role or verbatim Markdown", string(body))
	}
	if message.Content != summary {
		t.Fatal("model framing leaked into canonical summary", message.Content)
	}
}
