package history

import (
	"encoding/json"
	"fmt"
	"testing"

	"ttc/internal/llm"
)

func TestChildResponseMatchesAcceptedRawMessageFields(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	target, err := s.Append(session.ID, turn, "main/child_fixture", "message", "assistant", false,
		llm.Message{Type: llm.TypeAssistant, Content: "Committed child response"})
	if err != nil {
		t.Fatal(err)
	}
	completion := fmt.Sprintf(`{"type":"child_turn_finished","child_id":"main/child_fixture","result_entry_id":%d}`, target)
	quoted, err := json.Marshal(completion)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, fields string
		typ          llm.MessageType
		content      string
		want         int64
	}{
		{"canonical notification", `"type":"runtime_notification","content":` + string(quoted), llm.TypeRuntimeNotification, completion, target},
		{"duplicate human discriminator", `"type":"runtime_notification","type":"user_input","content":` + string(quoted), llm.TypeUserInput, completion, 0},
		{"duplicate notification discriminator", `"type":"user_input","type":"runtime_notification","content":` + string(quoted), llm.TypeRuntimeNotification, completion, target},
		{"uppercase discriminator", `"Type":"runtime_notification","content":` + string(quoted), llm.TypeRuntimeNotification, completion, target},
		{"mixed duplicate human discriminator", `"type":"runtime_notification","TYPE":"user_input","content":` + string(quoted), llm.TypeUserInput, completion, 0},
		{"mixed duplicate notification discriminator", `"TYPE":"user_input","type":"runtime_notification","content":` + string(quoted), llm.TypeRuntimeNotification, completion, target},
		{"null final discriminator", `"type":"runtime_notification","TYPE":null,"content":` + string(quoted), llm.TypeRuntimeNotification, completion, target},
		{"null initial discriminator", `"Type":null,"type":"runtime_notification","content":` + string(quoted), llm.TypeRuntimeNotification, completion, target},
		{"human discriminator then null", `"type":"user_input","TYPE":null,"content":` + string(quoted), llm.TypeUserInput, completion, 0},
		{"uppercase content", `"type":"runtime_notification","Content":` + string(quoted), llm.TypeRuntimeNotification, completion, target},
		{"duplicate completion content last", `"type":"runtime_notification","content":"not a completion","content":` + string(quoted), llm.TypeRuntimeNotification, completion, target},
		{"duplicate unrelated content last", `"type":"runtime_notification","content":` + string(quoted) + `,"content":"not a completion"`, llm.TypeRuntimeNotification, "not a completion", 0},
		{"mixed duplicate completion content last", `"type":"runtime_notification","content":"not a completion","CONTENT":` + string(quoted), llm.TypeRuntimeNotification, completion, target},
		{"mixed duplicate unrelated content last", `"type":"runtime_notification","Content":` + string(quoted) + `,"content":"not a completion"`, llm.TypeRuntimeNotification, "not a completion", 0},
		{"null final content", `"type":"runtime_notification","content":` + string(quoted) + `,"Content":null`, llm.TypeRuntimeNotification, completion, target},
		{"null initial content", `"type":"runtime_notification","CONTENT":null,"content":` + string(quoted), llm.TypeRuntimeNotification, completion, target},
		{"empty final content then null", `"type":"runtime_notification","content":` + string(quoted) + `,"CONTENT":"","Content":null`, llm.TypeRuntimeNotification, "", 0},
		{"only null content", `"type":"runtime_notification","Content":null,"CONTENT":null`, llm.TypeRuntimeNotification, "", 0},
		{"escaped field names", `"\u0074ype":"runtime_notification","\u0043ontent":` + string(quoted), llm.TypeRuntimeNotification, completion, target},
		{"human-authored completion JSON", `"Type":"user_input","Content":` + string(quoted), llm.TypeUserInput, completion, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := json.RawMessage("{" + test.fields + "}")
			var canonical llm.Message
			if err := json.Unmarshal(raw, &canonical); err != nil || canonical.Type != test.typ || canonical.Content != test.content {
				t.Fatalf("invalid accepted-envelope fixture: %+v, %v", canonical, err)
			}
			id, err := s.Append(session.ID, turn, "main", "message", "user", true, raw)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.ChildResponse(id)
			if err != nil || got != test.want {
				t.Fatalf("canonical type=%s content=%q: response=%d, want %d; %v", canonical.Type, canonical.Content, got, test.want, err)
			}
		})
	}
}
