package history

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"ttc/internal/llm"
	"ttc/internal/messagestream"
	"ttc/internal/render"
)

const forkViewChild = "main/child_fork_view"

type forkViewFixture struct {
	store   *Store
	source  Session
	turn    string
	initial int64
	request int64
	runtime []CommittedMessage
	header  messagestream.Item
}

func newForkViewFixture(t *testing.T) forkViewFixture {
	t.Helper()
	s, source, turn := usageFixture(t)
	fixture := forkViewFixture{store: s, source: source, turn: turn, initial: source.EntryTip}
	admitted := admitUsage(t, s, source, turn, true)
	fixture.request = admitted.RequestID
	items, err := s.RecordBoundary(source.ID, turn, "main", admitted.RequestID, "fork-view", llm.BoundaryEvent{
		Attempt: 1, Direction: "request", Payload: json.RawMessage(`{"frozen":"request"}`), Inspection: testInspection("Frozen request evidence"),
	})
	if err != nil || len(items) != 1 {
		t.Fatal("record frozen request", items, err)
	}
	fixture.header = items[0]
	_, calls, err := s.Assistant(source.ID, turn, "main", admitted.RequestID, llm.Message{Type: llm.TypeAssistant, Calls: []llm.ToolCall{
		{ID: "read", Name: "read", Arguments: json.RawMessage(`{"path":"fixture.txt"}`)},
	}})
	if err != nil || len(calls) != 1 {
		t.Fatal("record shared tool call", calls, err)
	}
	if _, err := s.CallResult(source.ID, turn, "main", calls[0], json.RawMessage(`{"ok":true,"content":"shared tool evidence"}`), nil, nil,
		render.Markdown{Revision: 1, Summary: "Shared tool result", Detail: "Shared tool inspection evidence"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(source.ID, turn, "main", llm.Message{Type: llm.TypeAssistant, Content: "Shared assistant evidence"}); err != nil {
		t.Fatal(err)
	}
	for _, message := range []llm.Message{
		{Type: llm.TypeRuntimeInstruction, Content: "Source runtime instructions remain inspectable"},
		{Type: llm.TypeRuntimeNotification, Content: `{"type":"wakeup","wakeup_id":"frozen","message":"Source runtime notice remains inspectable"}`},
	} {
		committed, err := s.AppendMessage(source.ID, turn, "main", message)
		if err != nil {
			t.Fatal(err)
		}
		fixture.runtime = append(fixture.runtime, committed)
	}
	if _, err := s.AppendMessage(source.ID, "", forkViewChild, llm.Message{Type: llm.TypeAssistant, Content: "Shared private child evidence"}); err != nil {
		t.Fatal(err)
	}
	finishUsage(t, s, admitted.RequestID, "completed", &llm.Usage{InputTokens: 123, OutputTokens: 7})
	if err := s.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	fixture.source, err = s.Session(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

// Counts deliberately inspect only a disposable synthetic store, never a live
// database. Ordinary Load may add one session, not historical payload/owner rows.
func forkViewStorageCounts(t *testing.T, s *Store) map[string]int {
	t.Helper()
	rows, err := s.db.Query("SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM "` + strings.ReplaceAll(table, `"`, `""`) + `"`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts[table] = count
	}
	return counts
}

func forkViewLoadWithoutCopies(t *testing.T, s *Store, source Session) Session {
	t.Helper()
	before := forkViewStorageCounts(t, s)
	fork, err := s.Load(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	after := forkViewStorageCounts(t, s)
	before["sessions"]++
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("ordinary fork copied historical storage: got %v, want %v", after, before)
	}
	if fork.ID == source.ID || fork.EntryTip != source.EntryTip || fork.UndoFloor != source.EntryTip || fork.FileTip != 0 || fork.LineageID != source.LineageID {
		t.Fatalf("immediate shared-prefix fork has wrong baseline: source=%+v fork=%+v", source, fork)
	}
	return fork
}

type forkViewSnapshot struct {
	context            ContextState
	replay             Replay
	tree               BranchTree
	stream             []messagestream.Item
	main, child, exact []byte
	conversation       []byte
}

func readForkViewSnapshot(t *testing.T, s *Store, session string) forkViewSnapshot {
	t.Helper()
	ctx := context.Background()
	saved, err := s.Session(session)
	if err != nil {
		t.Fatal(err)
	}
	var view forkViewSnapshot
	view.context, err = s.ReadContextState(ctx, session)
	if err != nil {
		t.Fatal("fork context", err)
	}
	view.replay, err = s.ReadReplay(ctx, session, 0)
	if err != nil {
		t.Fatal("fork replay", err)
	}
	view.tree, err = s.HistoryTree(ctx, session)
	if err != nil {
		t.Fatal("fork tree", err)
	}
	view.stream, err = s.StreamItems(ctx, saved.EntryTip)
	if err != nil {
		t.Fatal("fork stream", err)
	}
	view.main, err = s.ActorTranscript(session, "main", saved.EntryTip)
	if err != nil {
		t.Fatal("fork main export", err)
	}
	view.child, err = s.ActorTranscript(session, forkViewChild, saved.EntryTip)
	if err != nil {
		t.Fatal("fork child export", err)
	}
	view.exact, err = s.ActorTranscriptJSONL(session, "main", saved.EntryTip)
	if err != nil {
		t.Fatal("fork exact export", err)
	}
	view.conversation, err = s.Transcript(session, 0)
	if err != nil {
		t.Fatal("fork conversation export", err)
	}
	return view
}

func TestForkViewsImmediateNestedLoadSharesRowsAndRetainsCanonicalInspection(t *testing.T) {
	fixture := newForkViewFixture(t)
	s := fixture.store
	sourceContext, err := s.ReadContextState(context.Background(), fixture.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	inspections := make(map[int64]Inspection)
	for _, original := range fixture.runtime {
		inspection, err := s.ReadInspection(context.Background(), original.EntryID)
		if err != nil {
			t.Fatal(err)
		}
		inspections[original.EntryID] = inspection
	}
	fork := forkViewLoadWithoutCopies(t, s, fixture.source)
	nested := forkViewLoadWithoutCopies(t, s, fork)
	for _, session := range []Session{fork, nested} {
		view := readForkViewSnapshot(t, s, session.ID)
		if view.tree.Current != fixture.source.EntryTip || len(view.tree.Nodes) < 2 || len(view.replay.Rows) == 0 {
			t.Fatalf("immediate fork lost inherited presentation: %+v", session)
		}
		if len(view.stream) != 1 || !reflect.DeepEqual(view.stream[0], fixture.header) {
			t.Fatalf("immediate fork changed frozen request revision: %+v", view.stream)
		}
		if !strings.Contains(string(view.main), "Shared assistant evidence") || !strings.Contains(string(view.child), "Shared private child evidence") {
			t.Fatal("immediate fork lost selected actor evidence")
		}
		if !reflect.DeepEqual(view.context.Usage, sourceContext.Usage) || view.context.Usage.Reported == nil || view.context.Usage.Reported.RequestID != fixture.request {
			t.Fatalf("immediate fork lost inherited accounting: %+v", view.context.Usage)
		}
		var expected []llm.Message
		for _, message := range sourceContext.Messages {
			if !message.Type.IsRuntime() {
				expected = append(expected, message)
			}
		}
		if !reflect.DeepEqual(view.context.Messages, expected) {
			t.Fatalf("fork model input did not mask only inherited runtime messages: got %+v want %+v", view.context.Messages, expected)
		}
		for _, original := range fixture.runtime {
			canonical, err := s.ReadCommittedMessage(context.Background(), original.EntryID)
			if err != nil || !reflect.DeepEqual(canonical, original) {
				t.Fatalf("fork model masking changed canonical runtime evidence: got %+v want %+v err=%v", canonical, original, err)
			}
			inspection, err := s.ReadInspection(context.Background(), original.EntryID)
			if err != nil || !reflect.DeepEqual(inspection, inspections[original.EntryID]) {
				t.Fatalf("runtime inspection lost canonical content: %+v err=%v", inspection, err)
			}
			found := false
			for _, row := range view.replay.Rows {
				if row.ID == original.EntryID {
					found = row.Message != nil && reflect.DeepEqual(*row.Message, original.Message)
				}
			}
			if !found {
				t.Fatalf("fork replay lost masked but inspectable runtime message #%d", original.EntryID)
			}
		}
	}
	unchanged, err := s.ReadContextState(context.Background(), fixture.source.ID)
	if err != nil || !reflect.DeepEqual(unchanged, sourceContext) {
		t.Fatal("fork runtime masking mutated source input", err)
	}
}

func TestForkViewsFreezeSourceAppendBranchSwitchAndCompaction(t *testing.T) {
	fixture := newForkViewFixture(t)
	s := fixture.store
	pendingTurn, err := s.BeginTurn(fixture.source.ID, "async", fixture.source.Model)
	if err != nil {
		t.Fatal(err)
	}
	pending := admitUsage(t, s, fixture.source, pendingTurn, true)
	fixture.source, err = s.Session(fixture.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	fork := forkViewLoadWithoutCopies(t, s, fixture.source)
	nested := forkViewLoadWithoutCopies(t, s, fork)
	before := map[string]forkViewSnapshot{
		fork.ID: readForkViewSnapshot(t, s, fork.ID), nested.ID: readForkViewSnapshot(t, s, nested.ID),
	}
	assertFrozen := func(stage string) {
		t.Helper()
		for _, session := range []Session{fork, nested} {
			after := readForkViewSnapshot(t, s, session.ID)
			want := before[session.ID]
			for _, field := range []struct {
				name      string
				got, want any
			}{
				{"context messages", after.context.Messages, want.context.Messages},
				{"usage accounting", after.context.Usage, want.context.Usage},
				{"replay", after.replay, want.replay},
				{"tree", after.tree, want.tree},
				{"stream", after.stream, want.stream},
				{"main export", after.main, want.main},
				{"child export", after.child, want.child},
				{"exact actor export", after.exact, want.exact},
				{"conversation export", after.conversation, want.conversation},
			} {
				if !reflect.DeepEqual(field.got, field.want) {
					t.Errorf("%s changed frozen fork %s %s", stage, session.ID, field.name)
				}
			}
		}
	}
	// The request already exists at the fork cut, but its completion does not.
	// Neither mutable request status nor its later usage observation may leak.
	finishUsage(t, s, pending.RequestID, "completed", &llm.Usage{InputTokens: 777, OutputTokens: 77})
	if err := s.FinishTurn(pendingTurn, "completed"); err != nil {
		t.Fatal(err)
	}
	assertFrozen("completion of a source request admitted before the fork")
	turn, _, err := s.AdmitTurn(fixture.source.ID, "user", fixture.source.Model, &llm.Message{Type: llm.TypeUserInput, Content: "Later source-only input"})
	if err != nil {
		t.Fatal(err)
	}
	later := admitUsage(t, s, fixture.source, turn, true)
	if _, err := s.AppendMessage(fixture.source.ID, turn, "main", llm.Message{Type: llm.TypeAssistant, Content: "Later source-only answer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(fixture.source.ID, "", forkViewChild, llm.Message{Type: llm.TypeAssistant, Content: "Later source-only child evidence"}); err != nil {
		t.Fatal(err)
	}
	items, err := s.RecordBoundary(fixture.source.ID, fixture.turn, "main", fixture.request, "fork-view", llm.BoundaryEvent{
		Attempt: 1, Direction: "response", Payload: json.RawMessage(`{"later":"response"}`), Inspection: testInspection("Later source-only response"),
	})
	if err != nil || len(items) != 1 || items[0].Revision <= fixture.header.Revision {
		t.Fatal("source response did not advance the inherited header", items, err)
	}
	finishUsage(t, s, later.RequestID, "completed", &llm.Usage{InputTokens: 999, OutputTokens: 99})
	if err := s.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	assertFrozen("source append and late request revision")
	plan, err := s.PlanBranch(fixture.source.ID, fixture.initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CommitRestore(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(fixture.source.ID, "", "main", llm.Message{Type: llm.TypeAssistant, Content: "Source-only alternate branch"}); err != nil {
		t.Fatal(err)
	}
	assertFrozen("source branch switch")
	compaction, err := s.ReadCompaction(context.Background(), fixture.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := compaction.Retain(len(compaction.Messages()), nil, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.ResolveCompaction(context.Background(), retained, llm.NewCompactionContext("Source-only compacted context"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitCompaction(context.Background(), resolved); err != nil {
		t.Fatal(err)
	}
	assertFrozen("source compaction")
	// Even a new fork-of-fork with no local rows must use the frozen prefix,
	// not the parent's source session's now-selected branch or continuation.
	lateNested := forkViewLoadWithoutCopies(t, s, fork)
	late := readForkViewSnapshot(t, s, lateNested.ID)
	want := before[fork.ID]
	want.tree.SessionID = lateNested.ID
	if !reflect.DeepEqual(late, want) {
		t.Fatal("fork-of-fork after source compaction lost its frozen inherited view")
	}
}

func TestForkViewsMaskInheritedRuntimeButKeepForkLocalRuntime(t *testing.T) {
	s, source, turn := usageFixture(t)
	inherited, err := s.AppendMessage(source.ID, turn, "main", llm.Message{Type: llm.TypeRuntimeInstruction, Content: "Original runtime evidence"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	source, err = s.Session(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	fork := forkViewLoadWithoutCopies(t, s, source)
	local, err := s.AppendMessage(fork.ID, "", "main", llm.Message{Type: llm.TypeRuntimeInstruction, Content: "Fork-local runtime input"})
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.ReadContextState(context.Background(), fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundLocal := false
	for _, message := range current.Messages {
		if message.Content == inherited.Message.Content {
			t.Fatal("inherited runtime context entered fork model input")
		}
		foundLocal = foundLocal || reflect.DeepEqual(message, local.Message)
	}
	if !foundLocal {
		t.Fatal("fork-local runtime input was masked as inherited")
	}
	fork, err = s.Session(fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	nested := forkViewLoadWithoutCopies(t, s, fork)
	projection, err := s.ReadContextState(context.Background(), nested.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range projection.Messages {
		if message.Type.IsRuntime() {
			t.Fatalf("nested fork inherited a parent's runtime input: %s", message.Type)
		}
	}
	replay, err := s.ReadReplay(context.Background(), nested.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, original := range []CommittedMessage{inherited, local} {
		canonical, err := s.ReadCommittedMessage(context.Background(), original.EntryID)
		if err != nil || !reflect.DeepEqual(canonical, original) {
			t.Fatal("nested masking mutated original runtime evidence", canonical, err)
		}
		found := false
		for _, row := range replay.Rows {
			found = found || row.ID == original.EntryID && row.Message != nil && reflect.DeepEqual(*row.Message, original.Message)
		}
		if !found {
			t.Fatalf("nested replay lost runtime evidence #%d", original.EntryID)
		}
	}
}

func TestForkViewsAcceptInheritedTipsAndRejectUnsharedSiblingTips(t *testing.T) {
	s, source, turn := usageFixture(t)
	ctx := context.Background()
	initial := source.EntryTip
	abandoned, err := s.AppendMessage(source.ID, turn, "main", llm.Message{Type: llm.TypeAssistant, Content: "Abandoned source sibling"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.PlanBranch(source.ID, source.EntryTip)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CommitRestore(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(source.ID, turn, "main", llm.Message{Type: llm.TypeAssistant, Content: "Selected inherited source branch"}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	source, err = s.Session(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	fork := forkViewLoadWithoutCopies(t, s, source)
	nested := forkViewLoadWithoutCopies(t, s, fork)
	sibling := forkViewLoadWithoutCopies(t, s, source)
	siblingLocal, err := s.AppendMessage(sibling.ID, "", "main", llm.Message{Type: llm.TypeAssistant, Content: "Sibling fork-local message"})
	if err != nil {
		t.Fatal(err)
	}
	sourceLater, err := s.AppendMessage(source.ID, "", "main", llm.Message{Type: llm.TypeAssistant, Content: "Source post-fork message"})
	if err != nil {
		t.Fatal(err)
	}
	forkLocal, err := s.AppendMessage(fork.ID, "", "main", llm.Message{Type: llm.TypeAssistant, Content: "Parent post-nested-fork message"})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []Session{fork, nested} {
		rejected := []int64{abandoned.EntryID, siblingLocal.EntryID, sourceLater.EntryID}
		if target.ID == nested.ID {
			rejected = append(rejected, forkLocal.EntryID)
		}
		tree, err := s.HistoryTree(ctx, target.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range tree.Nodes {
			for _, tip := range rejected {
				if node.ID == tip {
					t.Errorf("fork tree %s admitted unshared sibling/later node %d", target.ID, tip)
				}
			}
		}
		for _, read := range []struct {
			name string
			call func(int64) error
		}{
			{"replay", func(tip int64) error { _, err := s.ReadReplay(ctx, target.ID, tip); return err }},
			{"messages", func(tip int64) error { _, err := s.MessagesAt(target.ID, tip); return err }},
			{"transcript", func(tip int64) error { _, err := s.Transcript(target.ID, tip); return err }},
			{"actor export", func(tip int64) error { _, err := s.ActorTranscript(target.ID, "main", tip); return err }},
			{"exact actor export", func(tip int64) error { _, err := s.ActorTranscriptJSONL(target.ID, "main", tip); return err }},
			{"restore plan", func(tip int64) error { _, err := s.PlanBranch(target.ID, tip); return err }},
		} {
			if err := read.call(source.EntryTip); err != nil {
				t.Errorf("%s rejected inherited tip %d in %s: %v", read.name, source.EntryTip, target.ID, err)
			}
			if read.name != "restore plan" {
				if err := read.call(initial); err != nil {
					t.Errorf("%s rejected earlier inherited ancestor %d in %s: %v", read.name, initial, target.ID, err)
				}
			}
			for _, tip := range rejected {
				if err := read.call(tip); err == nil {
					t.Errorf("%s accepted unshared sibling/later tip %d in %s", read.name, tip, target.ID)
				}
			}
		}
	}
}
