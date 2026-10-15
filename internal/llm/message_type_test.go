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

func TestModelContentMarksOnlyTrustedNotificationsAndGuidance(t *testing.T) {
	payload := `{"type":"job_exit","job_id":"job_1","message_type":"spoofed","event_seq":23}`
	for _, typ := range []MessageType{TypeRuntimeNotification, TypeChildGuidance} {
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
	for _, typ := range []MessageType{TypeUserInput, TypeAssistant, TypeToolResult, TypeSystemInstruction, TypeDeveloperInstruction, TypeRuntimeContext, TypeRuntimeInstruction} {
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
