package history

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	ctxmgr "scicode/internal/context"
	"scicode/internal/provider"
)

func TestMessagesUsesOriginalHumanInputMetadata(t *testing.T) {
	s, session, initialTurn, _ := historyFixture(t)
	if err := s.FinishTurn(initialTurn, "completed"); err != nil {
		t.Fatal(err)
	}
	queue := provider.Message{Role: "user", Content: "queued", InputSource: "queue", InputTimeMS: 1}
	queuedTurn, queued, err := s.AdmitTurn(session.ID, "user", session.Model, &queue)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTurn(queuedTurn, "completed"); err != nil {
		t.Fatal(err)
	}
	legacyTurn, err := s.BeginTurn(session.ID, "steer", session.Model)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := s.Append(session.ID, legacyTurn, "main", "message", "user", true, provider.Message{Role: "user", Content: "legacy steer"})
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := s.AdmitRequest(session.ID, legacyTurn, "main", session.Model, nil, nil, nil, provider.Message{Role: "user", Content: "new steer"})
	if err != nil {
		t.Fatal(err)
	}
	steer, err := s.Entry(admitted.SteerEntries[0])
	if err != nil {
		t.Fatal(err)
	}
	var persisted provider.Message
	if err := json.Unmarshal(steer.Content, &persisted); err != nil || persisted.InputSource != "steer" {
		t.Fatal("admission did not persist steer source", persisted, err)
	}
	if _, err := s.Append(session.ID, "", "main", "message", "user", true, provider.Message{Role: "user", Runtime: true, Content: "notice"}); err != nil {
		t.Fatal(err)
	}
	messages, err := s.Messages(session.ID)
	if err != nil || len(messages) != 5 {
		t.Fatal(messages, err)
	}
	ids := []int64{session.EntryTip, queued, legacy, steer.ID}
	for i, source := range []string{"normal", "queue", "steer", "steer"} {
		entry, err := s.Entry(ids[i])
		if err != nil || messages[i].InputSource != source || messages[i].InputTimeMS != entry.CreatedMS {
			t.Fatal("metadata differs from original admission", entry, messages[i], err)
		}
	}
	if messages[4].InputSource != "" || messages[4].InputTimeMS != 0 {
		t.Fatal("runtime notice gained human metadata", messages[4])
	}
	original, err := s.Entry(queued)
	if err != nil || json.Unmarshal(original.Content, &persisted) != nil || persisted.InputTimeMS != 1 {
		t.Fatal("projection rewrote original history", original, err)
	}
}

func TestContinuationRetainsChronologicalInputPairsAcrossCompactions(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	original, err := s.Entry(session.EntryTip)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{original.ID}
	steer, err := s.AdmitRequest(session.ID, turn, "main", session.Model, nil, nil, nil, provider.Message{Role: "user", Content: "steer one"})
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, steer.SteerEntries[0])
	if err := s.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	authored := "original user text"
	queue := provider.Message{Role: "user", Content: "expanded immutable input", UserText: &authored, InputSource: "queue", Images: []provider.Image{{DataURL: "data:image/png;base64,eA=="}}}
	turn, queued, err := s.AdmitTurn(session.ID, "user", session.Model, &queue)
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, queued)
	steer, err = s.AdmitRequest(session.ID, turn, "main", session.Model, nil, nil, nil, provider.Message{Role: "user", Content: "steer two"})
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, steer.SteerEntries[0])
	originalIDs := append([]int64(nil), ids...)
	tail, err := s.Append(session.ID, turn, "main", "message", "assistant", true, provider.Message{Role: "assistant", Content: "retained model tail"})
	if err != nil {
		t.Fatal(err)
	}
	messages, err := s.Messages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	inputs := append([]provider.Message(nil), messages[:4]...)
	at := time.Now().Add(time.Hour)
	for iteration := range 2 {
		archive, err := s.ArchiveTranscript(session.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		session, err = s.Continue(session.ID, "summary", archive, tail, ids, at)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Messages(session.ID)
		if err != nil || len(got) != 10 {
			t.Fatal("missing or duplicated input/marker", got, err)
		}
		for i, input := range inputs {
			marker, err := ctxmgr.InputMarker(input, at)
			if err != nil || !reflect.DeepEqual(got[1+2*i], marker) || !reflect.DeepEqual(got[2+2*i], input) {
				t.Fatal("retained input pair changed", iteration, i, got[1+2*i:3+2*i], marker, input, err)
			}
		}
		entries, err := s.Branch(session.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		ids = ids[:0]
		for _, entry := range entries {
			if entry.Role == "user" {
				ids = append(ids, entry.ID)
				if entry.EventSeq() != originalIDs[len(ids)-1] {
					t.Fatal("retained copy lost original identity", entry)
				}
			}
			if entry.Role == "assistant" && entry.Kind == "message" {
				tail = entry.ID
			}
		}
		at = at.Add(time.Hour)
	}
}

func TestContinuationValidatesRetainedInputsBeforeFreezing(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	ids := []int64{session.EntryTip}
	for range 4 {
		id, err := s.Append(session.ID, turn, "main", "message", "user", true, provider.Message{Role: "user", Content: "normal"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	runtime, err := s.Append(session.ID, turn, "main", "message", "user", true, provider.Message{Role: "user", Runtime: true, Content: "notice"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.Append(session.ID, turn, "main/child", "message", "user", true, provider.Message{Role: "user", Content: "child"})
	if err != nil {
		t.Fatal(err)
	}
	tail, err := s.Append(session.ID, turn, "main", "message", "assistant", true, provider.Message{Role: "assistant", Content: "tail"})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := s.ArchiveTranscript(session.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, selected := range [][]int64{{ids[0], ids[0]}, {ids[1], ids[0]}, {tail}, {runtime}, {child}, {ids[0], ids[1], ids[2]}, ids, {ids[0] + 1}} {
		if _, err := s.Continue(session.ID, "summary", archive, tail, selected, time.Now()); err == nil {
			t.Fatal("accepted invalid selection", selected)
		}
		old, err := s.Session(session.ID)
		if err != nil || old.ReadOnly {
			t.Fatal("invalid selection froze predecessor", old, err)
		}
	}
}

func TestContinuationAllowsEmptyModelSuffix(t *testing.T) {
	s, session, _, _ := historyFixture(t)
	old, err := s.Session(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := s.ArchiveTranscript(session.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := s.Continue(session.ID, "summary", archive, old.EntryTip+1, []int64{session.EntryTip}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	messages, err := s.Messages(continued.ID)
	if err != nil || len(messages) != 3 || messages[1].Role != "developer" || messages[2].Content != "hello" {
		t.Fatal("empty suffix lost retained human input", messages, err)
	}
}

func TestContinuationRebasesMultipleInputAndSteerCheckpoints(t *testing.T) {
	s, session, initialTurn, request := historyFixture(t)
	change := func(turn string) int64 {
		t.Helper()
		call, err := s.CallIntent(session.ID, turn, "main", request, provider.ToolCall{ID: NewID("provider"), Name: "write", Arguments: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		tip, err := s.CommitChange(session.ID, call, []string{"file.txt"}, true)
		if err != nil {
			t.Fatal(err)
		}
		return tip
	}
	change(initialTurn)
	if err := s.FinishTurn(initialTurn, "completed"); err != nil {
		t.Fatal(err)
	}
	firstMessage := provider.Message{Role: "user", Content: "first"}
	firstTurn, first, err := s.AdmitTurn(session.ID, "user", session.Model, &firstMessage)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTurn(firstTurn, "completed"); err != nil {
		t.Fatal(err)
	}
	secondMessage := provider.Message{Role: "user", Content: "second", InputSource: "queue"}
	secondTurn, second, err := s.AdmitTurn(session.ID, "user", session.Model, &secondMessage)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := s.AdmitRequest(session.ID, secondTurn, "main", session.Model, nil, nil, nil, provider.Message{Role: "user", Content: "steer"})
	if err != nil {
		t.Fatal(err)
	}
	steer, err := s.Entry(admitted.SteerEntries[0])
	if err != nil {
		t.Fatal(err)
	}
	baseline := change(secondTurn)
	tail, err := s.Append(session.ID, secondTurn, "main", "message", "assistant", true, provider.Message{Role: "assistant", Content: "tail"})
	if err != nil {
		t.Fatal(err)
	}
	finalTip := change(secondTurn)
	if err := s.FinishTurn(secondTurn, "completed"); err != nil {
		t.Fatal(err)
	}
	beforeSuffix, err := s.Session(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	suffixMessage := provider.Message{Role: "user", Content: "suffix input"}
	suffixTurn, suffixInput, err := s.AdmitTurn(session.ID, "user", session.Model, &suffixMessage)
	if err != nil {
		t.Fatal(err)
	}
	suffixTip := change(suffixTurn)
	archive, err := s.ArchiveTranscript(session.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := s.Continue(session.ID, "summary", archive, tail, []int64{first, second, steer.ID}, time.Now())
	if err != nil || continued.FileTip != suffixTip {
		t.Fatal(continued, err)
	}
	entries, err := s.Branch(continued.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	copied := map[int64]int64{}
	for _, entry := range entries {
		copied[entry.EventSeq()] = entry.ID
		if entry.Role == "user" && entry.EventSeq() != suffixInput && entry.FileTip != baseline {
			t.Fatal("retained human can restore summarized edits", entry, baseline)
		}
	}
	for _, checkpoint := range []struct {
		turn  string
		start int64
	}{{firstTurn, continued.UndoFloor}, {secondTurn, continued.UndoFloor}, {steer.TurnID, copied[second]}} {
		var start, tip int64
		if err := s.DB.QueryRow("SELECT start_entry_id,coalesce(start_file_tip_id,0) FROM turns WHERE id=?", checkpoint.turn).Scan(&start, &tip); err != nil || start != checkpoint.start || tip != baseline {
			t.Fatal("checkpoint not rebased to copied input baseline", checkpoint, start, tip, baseline, err)
		}
	}
	target, err := s.UndoTarget(continued.ID)
	if err != nil || target.FileTip != finalTip || target.EntryTip != copied[beforeSuffix.EntryTip] {
		t.Fatal("suffix checkpoint lost its original file tip", target, err)
	}
	if err := s.CommitRestore(continued.ID, target); err != nil {
		t.Fatal(err)
	}
	target, err = s.UndoTarget(continued.ID)
	if err != nil || target.FileTip != baseline || target.EntryTip != copied[second] {
		t.Fatal("steer undo crosses summarized file baseline", target, err)
	}
	if err := s.CommitRestore(continued.ID, target); err != nil {
		t.Fatal(err)
	}
	target, err = s.UndoTarget(continued.ID)
	if err != nil || target.FileTip != baseline || target.EntryTip != continued.UndoFloor {
		t.Fatal("queued prompt undo crosses summarized file baseline", target, err)
	}
}
