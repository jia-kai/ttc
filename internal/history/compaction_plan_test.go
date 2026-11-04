package history

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"ttc/internal/llm"
)

func TestCompactionPlanResolvesFrozenRetentionAndCommittedTail(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	ctx := context.Background()
	appendMessage := func(text string) int64 {
		t.Helper()
		id, err := s.Append(session.ID, turn, "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: text})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	appendMessage("summarized response")
	retained := appendMessage("retained response")
	view, err := s.ReadCompaction(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages := view.Messages()
	at := time.Now()
	plan, err := view.Retain(len(messages)-1, []int{0}, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Messages()) != 0 {
		t.Fatal("unresolved plan exposes replacement messages")
	}
	appendMessage("late committed response")
	if len(view.Messages()) != len(messages) {
		t.Fatal("frozen view included later arrivals")
	}
	handoff := llm.NewCompactionContext("checked summary")
	resolved, err := s.ResolveCompaction(ctx, plan, handoff, nil)
	if err != nil {
		t.Fatal(err)
	}
	assembled := resolved.Messages()
	if len(assembled) != 4 || assembled[0].Content != handoff.Content || assembled[2].Content != "retained response" || assembled[3].Content != "late committed response" {
		t.Fatal("invalid replacement input", assembled)
	}
	continued, err := s.CommitCompaction(ctx, resolved)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Messages(continued.ID)
	if err != nil || !reflect.DeepEqual(got, assembled) {
		t.Fatal("capacity-checked input differs from committed input", got, assembled, err)
	}
	var source, firstRetained int64
	if err := s.db.QueryRow("SELECT source_tip_id FROM compactions WHERE continuation_id=?", continued.ID).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != view.tip || resolved.retainFrom != retained {
		t.Fatal("wrong private source/suffix mapping", source, view.tip, resolved.retainFrom, retained)
	}
	if err := s.db.QueryRow("SELECT source_id FROM entries WHERE session_id=? AND source_id=?", continued.ID, retained).Scan(&firstRetained); err != nil {
		t.Fatal(err)
	}
}

func TestCompactionPlanEmptySuffixPreservesActualPostCutEntry(t *testing.T) {
	s, session, _, _ := historyFixture(t)
	ctx := context.Background()
	view, err := s.ReadCompaction(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := view.Retain(len(view.Messages()), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	foreign := NewID("session")
	if _, _, err := s.StartSession(foreign, t.TempDir(), session.Model, llm.Message{Type: llm.TypeUserInput, Content: "unrelated global ID gap"}); err != nil {
		t.Fatal(err)
	}
	postCut, err := s.Append(session.ID, "", "main", "status", "", false, map[string]string{"type": "selected_postcut_evidence"})
	if err != nil {
		t.Fatal(err)
	}
	if postCut <= view.tip+1 {
		t.Fatal("fixture lacks unrelated global ID gap")
	}
	resolved, err := s.ResolveCompaction(ctx, plan, llm.NewCompactionContext("summary"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.retainFrom != postCut {
		t.Fatal("empty suffix used arithmetic rather than selected arrival", resolved.retainFrom, postCut)
	}
	continued, err := s.CommitCompaction(ctx, resolved)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM entries WHERE session_id=? AND source_id=?", continued.ID, postCut).Scan(&count); err != nil || count != 1 {
		t.Fatal("lost hidden post-cut arrival", count, err)
	}
}

func TestCompactionPlanRejectsInvalidAndStalePlans(t *testing.T) {
	s, session, _, _ := historyFixture(t)
	ctx := context.Background()
	for _, expectedTip := range []int64{0, -1} {
		if _, err := s.continueAt(session.ID, llm.NewCompactionContext("summary"), session.EntryTip, session.EntryTip+1, nil, time.Now(), nil, expectedTip); err == nil {
			t.Fatal("persistence accepted a missing selected-tip fence", expectedTip)
		}
	}
	view, err := s.ReadCompaction(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, indices := range []struct {
		start  int
		inputs []int
	}{{-1, nil}, {2, nil}, {0, []int{0}}, {1, []int{0, 0}}} {
		if _, err := view.Retain(indices.start, indices.inputs, time.Now()); err == nil {
			t.Fatal("invalid retention accepted", indices)
		}
	}
	if _, err := (CompactionView{}).Retain(0, nil, time.Now()); err == nil {
		t.Fatal("zero view accepted")
	}
	plan, err := view.Retain(len(view.Messages()), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitCompaction(ctx, plan); err == nil {
		t.Fatal("unresolved plan committed")
	}
	resolved, err := s.ResolveCompaction(ctx, plan, llm.NewCompactionContext("summary"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(session.ID, "", "main", "status", "", false, map[string]string{"type": "later_commit"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitCompaction(ctx, resolved); !errors.Is(err, ErrCompactionPlanChanged) {
		t.Fatal("stale plan committed", err)
	}
	old, err := s.Session(session.ID)
	if err != nil || old.ReadOnly {
		t.Fatal("failed plan froze predecessor", old, err)
	}
	var continuations int
	if err := s.db.QueryRow("SELECT count(*) FROM sessions WHERE predecessor_id=?", session.ID).Scan(&continuations); err != nil || continuations != 0 {
		t.Fatal("failed plan created continuation", continuations, err)
	}
	resolved, err = s.ResolveCompaction(ctx, plan, llm.NewCompactionContext("summary"), nil)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.CommitCompaction(canceled, resolved); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled plan committed", err)
	}
}
