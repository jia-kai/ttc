package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"ttc/internal/historytest"

	"ttc/internal/llm"
	"ttc/internal/scratch"
	"ttc/internal/tool"
)

func overlapAppend(t *testing.T, r *Runtime, turn, actor string, visible bool, message llm.Message) int64 {
	t.Helper()
	id, err := r.Store.Append(r.Current(), turn, actor, "message", message.Role(), visible, message)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func overlapExport(t *testing.T, r *Runtime, actor, scope string) (ConversationExportResult, string) {
	t.Helper()
	root, err := scratch.Verify()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "conversation-overlap-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	value, err := r.exportConversation(context.Background(), tool.Execution{Actor: actor}, conversationExportArgs{
		Path: filepath.Join(dir, "history.jsonl"), Format: "jsonl", Scope: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := value.(ConversationExportResult)
	if !ok {
		t.Fatalf("unexpected export result %T", value)
	}
	body, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	return result, string(body)
}

func TestConversationExportAsideFrozenPrefixBehindMainHandoff(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	compactionBudget(t, r)
	seedCompactionHistory(t, r, strings.Repeat("ASIDE_FROZEN_MAIN_EVIDENCE ", 400))
	frozen, err := r.CurrentSession()
	if err != nil {
		t.Fatal(err)
	}
	actor := "main/btw_export_overlap"
	turn, err := r.Store.BeginChildTurn(frozen.ID, actor, r.CurrentSelection())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Store.RecordActorPrefix(frozen.ID, turn, actor, frozen.ID, frozen.EntryTip); err != nil {
		t.Fatal(err)
	}
	overlapAppend(t, r, turn, actor, false, llm.Message{Type: llm.TypeUserInput, Content: "ASIDE_ORIGINAL_QUESTION"})
	overlapAppend(t, r, "", "main", true, llm.Message{Type: llm.TypeAssistant, Content: "MAIN_AFTER_ASIDE_FREEZE"})
	overlapAppend(t, r, "", "main", true, llm.Message{Type: llm.TypeUserInput, Content: "Continue main research", InputTimeMS: time.Now().UnixMilli()})
	overlapAppend(t, r, "", "main/child_unrelated", false, llm.Message{Type: llm.TypeAssistant, Content: "UNRELATED_CHILD_SECRET"})
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		if !req.NoTools {
			return fmt.Errorf("unexpected coding request")
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "MAIN_HANDOFF_NOT_ADMITTED_TO_ASIDE"})
	}}
	if _, err := r.compactContext(context.Background(), "", r.CurrentSelection(), nil); err != nil {
		t.Fatal(err)
	}
	if r.Current() == frozen.ID {
		t.Fatal("main compaction did not hand off")
	}
	// The descriptor is not simply copied into the continuation. Selection must
	// find it by traversing the main summary's original ancestry.
	branch, err := r.Store.Branch(r.Current(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range branch {
		if strings.Contains(string(entry.Content), `"type":"actor_context_prefix"`) {
			t.Fatal("fixture did not move the frozen prefix behind summary ancestry")
		}
	}
	overlapAppend(t, r, turn, actor, false, llm.Message{Type: llm.TypeAssistant, Content: "ASIDE_SURVIVED_MAIN_HANDOFF"})
	_, text := overlapExport(t, r, actor, "full")
	for _, want := range []string{"ASIDE_FROZEN_MAIN_EVIDENCE", "ASIDE_ORIGINAL_QUESTION", "ASIDE_SURVIVED_MAIN_HANDOFF"} {
		if !strings.Contains(text, want) {
			t.Fatalf("aside export lost %q", want)
		}
	}
	for _, excluded := range []string{"MAIN_AFTER_ASIDE_FREEZE", "MAIN_HANDOFF_NOT_ADMITTED_TO_ASIDE", "UNRELATED_CHILD_SECRET"} {
		if strings.Contains(text, excluded) {
			t.Fatalf("aside export admitted later or unrelated data %q", excluded)
		}
	}
}

func TestConversationExportChildCompactionOverlapsMainHandoff(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	compactionBudget(t, r)
	seedCompactionHistory(t, r, strings.Repeat("Main research evidence. ", 400))
	before := r.Current()
	actor := "main/child_export_overlap"
	previousTurn, err := r.Store.BeginChildTurn(before, actor, r.CurrentSelection())
	if err != nil {
		t.Fatal(err)
	}
	previousTip := overlapAppend(t, r, previousTurn, actor, false, llm.Message{Type: llm.TypeAssistant, Content: "PRIOR_CHILD_ASSIGNMENT_EVIDENCE"})
	if err := r.Store.RecordActorCompaction(before, previousTurn, actor, before, previousTip); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.FinishTurn(previousTurn, "completed"); err != nil {
		t.Fatal(err)
	}
	turn, err := r.Store.BeginChildTurn(before, actor, r.CurrentSelection())
	if err != nil {
		t.Fatal(err)
	}
	messages := []llm.Message{
		{Type: llm.TypeUserInput, Content: "Current child assignment", InputSource: "task", InputTimeMS: time.Now().Add(-time.Minute).UnixMilli()},
		{Type: llm.TypeAssistant, Content: strings.Repeat("CURRENT_CHILD_FROZEN_EVIDENCE ", 400)},
		{Type: llm.TypeUserInput, Content: "Continue child investigation", InputSource: "task", InputTimeMS: time.Now().UnixMilli()},
	}
	var frozenTip int64
	for _, message := range messages {
		frozenTip = overlapAppend(t, r, turn, actor, false, message)
	}
	started, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		if !req.NoTools {
			return fmt.Errorf("unexpected coding request")
		}
		text := "Main handoff during child summary."
		if strings.Contains(req.ConversationID, actor) {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			text = "Child handoff after main continuation."
		} else {
			// Commit after main freezes its summary cut but before its routing
			// handoff. Continue must copy this tail into the new branch.
			if _, err := r.Store.Append(r.Current(), turn, actor, "message", "assistant", false,
				llm.Message{Type: llm.TypeAssistant, Content: "CHILD_COMMIT_DURING_MAIN_SUMMARY"}); err != nil {
				return err
			}
		}
		return emit(llm.StreamEvent{Kind: "text", Text: text})
	}}
	finished := make(chan error, 1)
	go func() {
		_, _, err := r.compactChild(ctx, childTask{actor: actor, turn: turn, selection: r.CurrentSelection(), tools: r.Tools}, messages, contextCursor{}, nil)
		finished <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("child summary did not start", ctx.Err())
	}
	if _, err := r.compactContext(ctx, "", r.CurrentSelection(), nil); err != nil {
		t.Fatal(err)
	}
	if r.Current() == before {
		t.Fatal("main handoff did not overlap the blocked child summary")
	}
	var mainTip int64
	if err := historytest.DB(t, r.Store).QueryRow("SELECT source_tip_id FROM compactions WHERE continuation_id=?", r.Current()).Scan(&mainTip); err != nil {
		t.Fatal(err)
	}
	if mainTip <= frozenTip {
		t.Fatal("fixture did not establish distinct main and child cuts", mainTip, frozenTip)
	}
	overlapAppend(t, r, turn, actor, false, llm.Message{Type: llm.TypeAssistant, Content: "LATER_CHILD_COMMIT_AFTER_MAIN_HANDOFF"})
	close(release)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("overlapping child handoff did not finish", ctx.Err())
	}
	current, err := r.CurrentSession()
	if err != nil {
		t.Fatal(err)
	}
	source, tip, err := r.Store.ActorCompactionTip(ctx, current.ID, actor, current.EntryTip)
	if err != nil || source != before || tip != frozenTip {
		t.Fatal("latest own compaction cut replaced by main or prior assignment cut", source, tip, frozenTip, err)
	}
	fullResult, full := overlapExport(t, r, actor, "full")
	cutResult, cut := overlapExport(t, r, actor, "before_compaction")
	if fullResult.SourceSession != current.ID || cutResult.SourceSession != before || cutResult.SourceTip != frozenTip {
		t.Fatal("export returned wrong source cuts", fullResult, cutResult)
	}
	for _, want := range []string{"PRIOR_CHILD_ASSIGNMENT_EVIDENCE", "CURRENT_CHILD_FROZEN_EVIDENCE"} {
		if !strings.Contains(full, want) || !strings.Contains(cut, want) {
			t.Fatalf("child export lost earlier assignment or frozen evidence %q", want)
		}
	}
	for _, later := range []string{"CHILD_COMMIT_DURING_MAIN_SUMMARY", "LATER_CHILD_COMMIT_AFTER_MAIN_HANDOFF"} {
		if !strings.Contains(full, later) || strings.Contains(cut, later) {
			t.Fatalf("full and before-compaction exports did not respect frozen child boundary for %q", later)
		}
	}
	if strings.Contains(full, "Main research evidence.") || strings.Contains(cut, "Main research evidence.") {
		t.Fatal("isolated child export admitted main history")
	}
}
