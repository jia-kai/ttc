package history

import (
	"reflect"
	"testing"

	"ttc/internal/llm"
)

func TestMessagesAtKeepsFrozenCutWhenVisibleArrivalsPrecedeProjection(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	before, err := s.Messages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	cut, err := s.Session(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	// These commits occur after the cut but before its model projection. They
	// must be handled as tail by compaction, not silently treated as frozen input.
	for _, message := range []llm.Message{
		{Type: llm.TypeAssistant, Content: "Visible work arriving after the cut."},
		{Type: llm.TypeRuntimeNotification, Content: `{"type":"job_exit","job_id":"later"}`},
	} {
		if _, err := s.Append(session.ID, turn, "main", "message", message.Role(), true, message); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Append(session.ID, turn, "main", "status", "", false, map[string]string{"type": "later_status"}); err != nil {
		t.Fatal(err)
	}
	frozen, err := s.MessagesAt(session.ID, cut.EntryTip)
	if err != nil || !reflect.DeepEqual(frozen, before) {
		t.Fatal("frozen model input acquired a later arrival or lost original provenance", frozen, before, err)
	}
	current, err := s.Messages(session.ID)
	if err != nil || len(current) != len(before)+2 {
		t.Fatal("current model input lost committed tail", len(current), err)
	}
	zero, err := s.MessagesAt(session.ID, 0)
	if err != nil || !reflect.DeepEqual(zero, current) {
		t.Fatal("zero tip disagrees with current input", err)
	}
	if _, err := s.MessagesAt(session.ID, -1); err == nil {
		t.Fatal("negative tip accepted")
	}
}
