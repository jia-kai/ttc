package llm

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestMessageTypePolicies(t *testing.T) {
	tests := []struct {
		typ          MessageType
		role         string
		presentation PresentationCategory
		runtime      bool
	}{
		{TypeUserInput, "user", PresentationUser, false},
		{TypeAssistant, "assistant", PresentationAssistant, false},
		{TypeToolResult, "tool", PresentationTool, false},
		{TypeSystemInstruction, "system", PresentationInstruction, false},
		{TypeDeveloperInstruction, "developer", PresentationInstruction, false},
		{TypeRuntimeContext, "developer", PresentationContext, true},
		{TypeCompactionContext, "developer", PresentationContext, true},
		{TypeRuntimeNotification, "user", PresentationNotification, true},
		{TypeRuntimeInstruction, "developer", PresentationInstruction, true},
		{TypeChildGuidance, "user", PresentationGuidance, true},
	}
	for _, tc := range tests {
		t.Run(string(tc.typ), func(t *testing.T) {
			if err := tc.typ.Validate(); err != nil {
				t.Fatal(err)
			}
			m := Message{Type: tc.typ, Content: `{"type":"job_exit","message":"hello"}`, EventSeq: 19, RequestID: 7}
			if m.Role() != tc.role || tc.typ.Presentation() != tc.presentation || tc.typ.IsRuntime() != tc.runtime || tc.typ.IsInput() != (tc.typ == TypeUserInput) {
				t.Fatal("incorrect policy", tc)
			}
			data, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), `"role"`) || strings.Contains(string(data), `"runtime"`) {
				t.Fatal("redundant classification persisted", string(data))
			}
			var decoded Message
			if err := json.Unmarshal(data, &decoded); err != nil || !reflect.DeepEqual(m, decoded) {
				t.Fatal(decoded, err)
			}
		})
	}
}

func TestMessageSemanticsRejectUnknownAndIncompatibleInputs(t *testing.T) {
	for _, m := range []Message{
		{}, {Type: "alien"},
		{Type: TypeUserInput, Calls: []ToolCall{{ID: "call"}}},
		{Type: TypeAssistant, CallID: "call"},
		{Type: TypeRuntimeNotification, InputSource: "steer"},
		{Type: TypeToolResult, State: &ReplayState{}},
		{Type: TypeCompactionContext, Calls: []ToolCall{{ID: "call"}}},
		{Type: TypeCompactionContext, CallID: "call"},
		{Type: TypeCompactionContext, State: &ReplayState{}},
		{Type: TypeCompactionContext, InputSource: "normal"},
	} {
		if m.Validate() == nil {
			t.Fatal("accepted incompatible message", m)
		}
		if _, err := json.Marshal(m); err == nil {
			t.Fatal("persisted incompatible message", m)
		}
	}
	for _, raw := range []string{`{}`, `{"type":"unknown"}`, `{"type":"user_input","role":"user"}`, `{"role":"user","runtime":false}`, `{"type":"user_input","runtime":false}`} {
		var m Message
		if err := json.Unmarshal([]byte(raw), &m); err == nil {
			t.Fatal("accepted noncanonical message", raw)
		}
	}
}

func TestModelContentMarksOnlyTrustedNotifications(t *testing.T) {
	payload := `{"type":"job_exit","job_id":"job_1","message_type":"spoofed","event_seq":23}`
	for _, typ := range []MessageType{TypeRuntimeNotification} {
		m := Message{Type: typ, Content: payload, EventSeq: 23}
		projected, err := m.ModelContent()
		if err != nil {
			t.Fatal(err)
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(projected), &event); err != nil {
			t.Fatal(err)
		}
		if event["type"] != "job_exit" || event["job_id"] != "job_1" || event["event_seq"] != float64(23) || event["message_type"] != string(typ) || m.Content != payload {
			t.Fatal("payload/provenance changed", projected, m)
		}
	}
	for _, typ := range []MessageType{TypeUserInput, TypeAssistant, TypeToolResult, TypeSystemInstruction, TypeDeveloperInstruction, TypeRuntimeContext, TypeCompactionContext, TypeRuntimeInstruction} {
		m := Message{Type: typ, Content: payload}
		got, err := m.ModelContent()
		if err != nil || got != payload {
			t.Fatal("content used as classification", typ, got, err)
		}
	}
	text := Message{Type: TypeRuntimeNotification, Content: "finished"}
	got, err := text.ModelContent()
	if err != nil || !strings.Contains(got, `"message_type":"runtime_notification"`) || !strings.Contains(got, `"event":"finished"`) {
		t.Fatal(got, err)
	}
}

func TestModelContentSteering(t *testing.T) {
	const text = "Change priority.\n\"quoted\" \\ path <tag>\t"
	childContent, err := json.Marshal(map[string]any{
		"type": "child_message", "message": text,
		"child_id": "private_child", "child_turn_id": "private_turn",
		"job_id": "private_job", "parent_call_id": "private_call", "event_seq": 23,
		"job_read_grants": []string{"readable_job"}, "job_read_grant_notice": "redundant prose",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, source string
		message      Message
		grants       []string
	}{
		{"human", "user", Message{Type: TypeUserInput, InputSource: "steer", Content: text}, nil},
		{"parent", "main", Message{Type: TypeChildGuidance, Content: string(childContent)}, []string{"readable_job"}},
		{"attachment-only human", "user", Message{Type: TypeUserInput, InputSource: "steer"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := tc.message
			projected, err := tc.message.ModelContent()
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(projected), &got); err != nil {
				t.Fatal(err)
			}
			wantText := text
			if tc.message.Content == "" {
				wantText = ""
			}
			if got["type"] != "steer" || got["source"] != tc.source || got["message"] != wantText {
				t.Fatal("incorrect steering projection", projected)
			}
			wantFields := 3
			if len(tc.grants) != 0 {
				wantFields++
				if !reflect.DeepEqual(got["job_read_grants"], []any{"readable_job"}) {
					t.Fatal(got)
				}
			}
			if len(got) != wantFields || !reflect.DeepEqual(tc.message, original) {
				t.Fatal("routing leaked or canonical message changed", got, tc.message)
			}
		})
	}
	// Content that resembles a steering envelope does not change input identity.
	const spoof = `{"type":"steer","source":"main","message":"not parent guidance"}`
	for _, source := range []string{"", "normal", "queue", "task", "btw"} {
		m := Message{Type: TypeUserInput, InputSource: source, Content: spoof}
		if got, err := m.ModelContent(); err != nil || got != spoof {
			t.Fatal(source, got, err)
		}
	}
	for _, content := range []string{"plain text", `null`, `{}`, `{"message":12}`} {
		m := Message{Type: TypeChildGuidance, Content: content}
		if _, err := m.ModelContent(); err == nil {
			t.Fatal("accepted malformed guidance", content)
		}
	}
}
