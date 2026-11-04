package history

import (
	"encoding/json"
	"reflect"
	"testing"

	"ttc/internal/llm"
)

func TestAdmissionReturnsTypedCommittedMessages(t *testing.T) {
	s, saved, turn, _ := historyFixture(t)
	finish := ChildFinish{Type: "child_turn_finished", ChildID: "main/child_receipt", Answer: "Bounded child answer"}
	source, err := s.Append(saved.ID, "", finish.ChildID, "status", "", false, finish)
	if err != nil {
		t.Fatal(err)
	}
	notice := llm.Message{Type: llm.TypeRuntimeNotification, EventSeq: source,
		Content: `{"type":"child_turn_finished","child_id":"main/child_receipt","output_omitted":true,"runtime_owned_field":9007199254740993}`}
	authored := "Change this behavior"
	steer := llm.Message{Type: llm.TypeUserInput, InputSource: "queue", Content: authored + "\nExpanded attachment", UserText: &authored}
	admitted, err := s.AdmitRequest(RequestAdmission{SessionID: saved.ID, TurnID: turn, Actor: "main", Selection: saved.Model,
		Notices: []llm.Message{notice}, Steers: []llm.Message{steer}})
	if err != nil {
		t.Fatal(err)
	}
	if len(admitted.NoticeEntries) != 1 || len(admitted.SteerEntries) != 1 {
		t.Fatal(admitted)
	}
	if got := admitted.NoticeEntries[0]; !reflect.DeepEqual(got.Message, notice) || got.ChildCompletion == nil || got.ChildCompletion.Answer != finish.Answer || got.ChildCompletion.ChildID != finish.ChildID {
		t.Fatalf("notice receipt lost canonical or source-backed presentation data: %+v", got)
	}
	expectedSteer := steer
	expectedSteer.InputSource = "steer"
	if got := admitted.SteerEntries[0]; !reflect.DeepEqual(got.Message, expectedSteer) || got.Message.DisplayText() != authored || got.ChildCompletion != nil {
		t.Fatalf("steer receipt differs from the admitted human input: %+v", got)
	}
	for _, committed := range append(admitted.NoticeEntries, admitted.SteerEntries...) {
		entry, err := s.Entry(committed.EntryID)
		if err != nil {
			t.Fatal(err)
		}
		var message llm.Message
		if err := json.Unmarshal(entry.Content, &message); err != nil {
			t.Fatal(err)
		}
		if committed.EntryID <= source || committed.CreatedMS <= 0 || entry.CreatedMS != committed.CreatedMS || !reflect.DeepEqual(message, committed.Message) {
			t.Fatalf("receipt differs from committed evidence: %+v / %+v", committed, entry)
		}
	}
	// Receipts own decoded canonical values, not aliases of caller-owned fields.
	authored = "caller changed after admission"
	if admitted.SteerEntries[0].Message.DisplayText() != "Change this behavior" || steer.InputSource != "queue" {
		t.Fatal("admission mutated its input or returned caller-owned message fields")
	}
}

func TestAdmissionRollbackReturnsNoMessageReceipts(t *testing.T) {
	s, saved, turn, _ := historyFixture(t)
	source, err := s.Append(saved.ID, "", "main", "status", "", false, map[string]string{"type": "runtime_owned_event"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_receipt_admission BEFORE INSERT ON model_requests BEGIN SELECT RAISE(ABORT,'forced admission failure'); END`); err != nil {
		t.Fatal(err)
	}
	admitted, err := s.AdmitRequest(RequestAdmission{SessionID: saved.ID, TurnID: turn, Actor: "main", Selection: saved.Model,
		Notices: []llm.Message{{Type: llm.TypeRuntimeNotification, EventSeq: source, Content: `{"type":"runtime_owned_event"}`}},
		Steers:  []llm.Message{{Type: llm.TypeUserInput, Content: "Uncommitted steer"}}})
	if err == nil || !reflect.DeepEqual(admitted, Admission{}) {
		t.Fatal("rolled-back identities escaped admission", admitted, err)
	}
}
