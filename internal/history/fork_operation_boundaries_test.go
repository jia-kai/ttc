package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"ttc/internal/llm"
)

func TestSharedPrefixRestoreTreeAndFileBaseline(t *testing.T) {
	s, source, turn, request := historyFixture(t)
	change := func(session Session, turn string, request int64, label string) int64 {
		t.Helper()
		call, err := s.CallIntent(session.ID, turn, "main", request, llm.ToolCall{ID: NewID("call"), Name: "write", Arguments: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		id, err := s.CommitChange(session.ID, call, []string{label}, true)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	sourceChange := change(source, turn, request, "source edit")
	cut, err := s.Session(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	fork, err := s.Load(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fork.FileTip != 0 || fork.EntryTip != cut.EntryTip || fork.UndoFloor != cut.EntryTip {
		t.Fatal("fork did not establish its independent file/undo baseline", fork, cut)
	}
	later, err := s.Append(source.ID, turn, "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: "Source after frozen cut"})
	if err != nil {
		t.Fatal(err)
	}
	selectSource, err := s.PlanBranch(source.ID, source.EntryTip)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CommitRestore(selectSource); err != nil {
		t.Fatal(err)
	}
	sibling, err := s.Append(source.ID, turn, "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: "Abandoned source sibling"})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := s.HistoryTree(context.Background(), fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundBase := false
	for _, node := range tree.Nodes {
		if node.ID == later || node.ID == sibling {
			t.Fatal("source branches leaked into fork tree", node)
		}
		if node.ID == cut.EntryTip {
			foundBase = node.Restorable && node.Selected
		}
	}
	if !foundBase {
		t.Fatal("frozen base is not a restorable selected node", tree)
	}
	base, err := s.PlanBranch(fork.ID, cut.EntryTip)
	if err != nil || base.after.file != 0 {
		t.Fatal("base restore inherited source file history", base, err)
	}
	if steps, err := s.RestoreChanges(base); err != nil || len(steps) != 0 {
		t.Fatal("base restore tried to apply source files", steps, err)
	}
	if _, err := s.PlanBranch(fork.ID, later); err == nil {
		t.Fatal("source future tip was accepted by fork restore")
	}
	if _, err := s.ActorTranscript(fork.ID, "main", later); err == nil {
		t.Fatal("source future tip was accepted by fork transcript")
	}
	transcript, err := s.ActorTranscript(fork.ID, "main", cut.EntryTip)
	if err != nil || !strings.Contains(string(transcript), "hello") || strings.Contains(string(transcript), "Source after") {
		t.Fatal("logical base transcript failed", string(transcript), err)
	}
	forkTurn, err := s.BeginTurn(fork.ID, "user", fork.Model)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(fork.ID, forkTurn, "main", llm.Message{Type: llm.TypeUserInput, Content: "Fork work"}); err != nil {
		t.Fatal(err)
	}
	forkRequest, err := s.StartRequest(fork.ID, forkTurn, "main", "coding", fork.Model)
	if err != nil {
		t.Fatal(err)
	}
	forkChange := change(fork, forkTurn, forkRequest, "fork edit")
	undo, err := s.PlanUndo(fork.ID)
	if err != nil || undo.after.entry != cut.EntryTip || undo.after.file != 0 {
		t.Fatal("fork undo crossed its imported baseline", undo, err)
	}
	steps, err := s.RestoreChanges(undo)
	if err != nil || len(steps) != 1 || !steps[0].Reverse || string(steps[0].Paths) != `["fork edit"]` {
		t.Fatal("fork undo included source file chain", steps, err)
	}
	if err := s.CommitRestore(undo); err != nil {
		t.Fatal(err)
	}
	redo, err := s.PlanRedo(fork.ID)
	if err != nil || redo.after.file != forkChange || redo.after.file == sourceChange {
		t.Fatal("fork redo lost its independent file chain", redo, err)
	}
	if err := s.CommitRestore(redo); err != nil {
		t.Fatal(err)
	}
	second, err := s.Load(fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondBase, err := s.PlanBranch(second.ID, second.EntryTip)
	if err != nil || secondBase.after.file != 0 {
		t.Fatal("fork-of-fork inherited its source's file baseline", secondBase, err)
	}
}

func TestSharedPrefixRecoveryInstructionRequiresEffectiveWarning(t *testing.T) {
	s, source, turn, _ := historyFixture(t)
	warning := llm.Message{Type: llm.TypeRuntimeInstruction, Content: "Recovery warning", RequestID: 42}
	first, _, err := s.EnsureRecoveryInstruction(source.ID, turn, "main", warning)
	if err != nil {
		t.Fatal(err)
	}
	previous := first.EntryID
	for generation := 0; generation < 2; generation++ {
		fork, err := s.Load(source.ID)
		if err != nil {
			t.Fatal(err)
		}
		committed, added, err := s.EnsureRecoveryInstruction(fork.ID, "", "main", warning)
		if err != nil || !added || committed.EntryID == previous {
			t.Fatal("hidden inherited instruction satisfied a live recovery warning", committed, added, err)
		}
		messages, err := s.Messages(fork.ID)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, message := range messages {
			if message.Type == warning.Type && message.Content == warning.Content {
				count++
			}
		}
		if count != 1 {
			t.Fatal("fork model projection contains inherited runtime warnings", count)
		}
		if duplicate, added, err := s.EnsureRecoveryInstruction(fork.ID, "", "main", warning); err != nil || added || duplicate.EntryID != committed.EntryID {
			t.Fatal("effective local warning was not deduplicated", duplicate, added, err)
		}
		source, previous = fork, committed.EntryID
	}
}

func TestSharedPrefixRecoveryNotificationsStayInLiveCompactionChain(t *testing.T) {
	s, source, turn, _ := historyFixture(t)
	original, err := s.Append(source.ID, turn, "main", "status", "", false, recoveryEvent{Type: "runtime_event", Body: json.RawMessage(`{"type":"job_exit","job_id":"source-job"}`)})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := s.Append(source.ID, turn, "main", "status", "", false, recoveryEvent{Type: "runtime_event", RecoveredFrom: original, Body: json.RawMessage(`{"type":"job_exit","job_id":"source-job"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(source.ID, turn, "main", "status", "", false, map[string]string{"type": "compaction_started"}); err != nil {
		t.Fatal(err)
	}
	fork, err := s.Load(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.PendingRecoveryNotifications(context.Background(), fork.ID)
	if err != nil || len(first) != 1 || first[0].EventSeq == recovered || first[0].RecoveredFrom != recovered {
		t.Fatal("first fork recovered an inherited original directly", first, err)
	}
	second, err := s.Load(fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.PendingRecoveryNotifications(context.Background(), second.ID)
	if err != nil || len(next) != 1 || next[0].RecoveredFrom != first[0].EventSeq {
		t.Fatal("fork-of-fork revived manual-base source events", next, err)
	}
	view, err := s.ReadCompaction(context.Background(), fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := view.Retain(len(view.Messages()), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	plan, err = s.ResolveCompaction(context.Background(), plan, llm.NewCompactionContext("Summary"), nil)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := s.CommitCompaction(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingRecoveryNotifications(context.Background(), continued.ID)
	if err != nil || len(pending) != 1 || pending[0].EventSeq != first[0].EventSeq {
		t.Fatal("live predecessor recovery was lost or manual ancestry leaked", pending, err)
	}
	if _, err := s.AdmitRequest(RequestAdmission{SessionID: continued.ID, Actor: "main", Selection: continued.Model,
		Notices: []llm.Message{{Type: llm.TypeRuntimeNotification, EventSeq: pending[0].EventSeq, Content: string(pending[0].Body)}}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{recovered, next[0].EventSeq} {
		var delivered bool
		if err := s.db.QueryRow("SELECT delivered_request_id IS NOT NULL FROM entries WHERE id=?", id).Scan(&delivered); err != nil || delivered {
			t.Fatal("live continuation admission acknowledged another runtime's source", id, delivered, err)
		}
	}
}

func TestSharedPrefixCompactionKeepsSourceTurnsAndZeroFileBaseline(t *testing.T) {
	s, source, turn, request := historyFixture(t)
	call, err := s.CallIntent(source.ID, turn, "main", request, llm.ToolCall{ID: NewID("call"), Name: "write", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitChange(source.ID, call, []string{"source edit"}, true); err != nil {
		t.Fatal(err)
	}
	audit, err := s.Append(source.ID, turn, "main", "status", "", false, map[string]any{"type": "request_message", "purpose": "compaction", "request_id": request, "message": "private compaction request"})
	if err != nil {
		t.Fatal(err)
	}
	fork, err := s.Load(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ownerBefore string
	var startBefore, fileBefore int64
	if err := s.db.QueryRow("SELECT session_id,coalesce(start_entry_id,0),coalesce(start_file_tip_id,0) FROM turns WHERE id=?", turn).Scan(&ownerBefore, &startBefore, &fileBefore); err != nil {
		t.Fatal(err)
	}
	localTurn, err := s.BeginTurn(fork.ID, "user", fork.Model)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(fork.ID, localTurn, "main", llm.Message{Type: llm.TypeUserInput, Content: "Local fork work"}); err != nil {
		t.Fatal(err)
	}
	view, err := s.ReadCompaction(context.Background(), fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := view.Retain(0, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	plan, err = s.ResolveCompaction(context.Background(), plan, llm.NewCompactionContext("Summary"), nil)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := s.CommitCompaction(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	var ownerAfter string
	var startAfter, fileAfter int64
	if err := s.db.QueryRow("SELECT session_id,coalesce(start_entry_id,0),coalesce(start_file_tip_id,0) FROM turns WHERE id=?", turn).Scan(&ownerAfter, &startAfter, &fileAfter); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]any{ownerBefore, startBefore, fileBefore}, []any{ownerAfter, startAfter, fileAfter}) {
		t.Fatal("fork compaction moved its source turn", ownerBefore, ownerAfter, startBefore, startAfter, fileBefore, fileAfter)
	}
	entries, err := s.Branch(continued.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Source == audit {
			t.Fatal("inherited main compaction request audit was copied into the continuation", entry)
		}
		if entry.FileTip != 0 {
			t.Fatal("compaction resurrected source file history", entry.FileTip)
		}
	}
	undo, err := s.PlanUndo(continued.ID)
	if err != nil || undo.after.file != 0 {
		t.Fatal("continued fork undo lost its zero file baseline", undo, err)
	}
	stillSource, err := s.Session(source.ID)
	if err != nil || stillSource.ReadOnly {
		t.Fatal("fork compaction froze the source", stillSource, err)
	}
}

func TestSharedPrefixRecoveryRetainsPostCutLiveEvents(t *testing.T) {
	s, source, turn, _ := historyFixture(t)
	sourceEvent, err := s.Append(source.ID, turn, "main", "status", "", false, recoveryEvent{Type: "runtime_event", Body: json.RawMessage(`{"type":"job_exit","job_id":"source-runtime"}`)})
	if err != nil {
		t.Fatal(err)
	}
	fork, err := s.Load(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A legacy physical copy is locally owned but still refers to the manual
	// source runtime's semantic original, so ownership filtering must reject it.
	if err := s.transact(func(tx *sql.Tx) error {
		_, err := appendTx(tx, fork.ID, "", "main", "status", "", false,
			json.RawMessage(`{"type":"runtime_event","body":{"type":"job_exit","job_id":"source-runtime"}}`), sourceEvent)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	view, err := s.ReadCompaction(context.Background(), fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := view.Retain(0, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// This original is later than the compaction's frozen summary cut. Its
	// copied retained tail, not the predecessor's summary ancestry, carries it.
	if _, err := s.StartRequest(fork.ID, "", "main", "compaction", fork.Model); err != nil {
		t.Fatal(err)
	}
	tail, err := s.Append(fork.ID, "", "main", "status", "", false, recoveryEvent{Type: "runtime_event", Body: json.RawMessage(`{"type":"job_exit","job_id":"fork-runtime"}`)})
	if err != nil {
		t.Fatal(err)
	}
	plan, err = s.ResolveCompaction(context.Background(), plan, llm.NewCompactionContext("Summary"), nil)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := s.CommitCompaction(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(continued.ID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingRecoveryNotifications(context.Background(), loaded.ID)
	if err != nil || len(pending) != 1 || pending[0].RecoveredFrom != tail || !strings.Contains(string(pending[0].Body), "fork-runtime") {
		t.Fatal("post-cut live arrival was lost or manual source runtime leaked", pending, err)
	}
	if _, err := s.AdmitRequest(RequestAdmission{SessionID: source.ID, TurnID: turn, Actor: "main", Selection: source.Model,
		Notices: []llm.Message{{Type: llm.TypeRuntimeNotification, EventSeq: sourceEvent, Content: `{"type":"job_exit","job_id":"source-runtime"}`}}}); err != nil {
		t.Fatal(err)
	}
	stillPending, err := s.PendingRecoveryNotifications(context.Background(), loaded.ID)
	if err != nil || len(stillPending) != 1 || stillPending[0].EventSeq != pending[0].EventSeq {
		t.Fatal("source acknowledgment changed the fork's delivery state", stillPending, err)
	}
	if _, err := s.AdmitRequest(RequestAdmission{SessionID: loaded.ID, Actor: "main", Selection: loaded.Model,
		Notices: []llm.Message{{Type: llm.TypeRuntimeNotification, EventSeq: pending[0].EventSeq, Content: string(pending[0].Body)}}}); err != nil {
		t.Fatal(err)
	}
	var delivered bool
	if err := s.db.QueryRow("SELECT delivered_request_id IS NOT NULL FROM entries WHERE id=?", tail).Scan(&delivered); err != nil || delivered {
		t.Fatal("new fork acknowledgment changed its original source event", delivered, err)
	}
}
