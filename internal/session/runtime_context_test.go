package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"scicode/internal/provider"
)

func TestRuntimeInstructionsRejectSpecialFilesAndCancellation(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	path := filepath.Join(r.Workspace.Root, "AGENTS.md")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.runtimeContext(context.Background(), "main", r.selection, contextCursor{}); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatal("special instruction accepted", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := r.runtimeContext(ctx, "main", r.selection, contextCursor{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not propagated", err)
	}
}

func TestRuntimeContextProjectChangesAndFinishedJobs(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Inspect runtime state")
	decode := func(m *provider.Message) runtimeContext {
		t.Helper()
		var v runtimeContext
		if m == nil || m.Role != "developer" || !m.Runtime {
			t.Fatal(m)
		}
		if err := json.Unmarshal([]byte(m.Content), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	m, cursor, err := r.runtimeContext(context.Background(), "main", r.selection, contextCursor{})
	if err != nil {
		t.Fatal(err)
	}
	if decode(m).Project == nil {
		t.Fatal("missing initial project context")
	}
	m, cursor, err = r.runtimeContext(context.Background(), "main", r.selection, cursor)
	if err != nil || m != nil {
		t.Fatal(m, err)
	}
	if err = os.WriteFile(filepath.Join(r.Workspace.Root, "AGENTS.md"), []byte("Use precise units."), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := r.Jobs.Start("main", "exit 7", r.Workspace.Root, 0, true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Jobs.Wait(context.Background(), "main", id, nil); err != nil {
		t.Fatal(err)
	}
	m, next, err := r.runtimeContext(context.Background(), "main", r.selection, cursor)
	v := decode(m)
	if err != nil || v.Project == nil || len(v.Jobs) != 0 || len(v.Changes) != 1 {
		t.Fatal(v, err)
	}
	change := v.Changes[0]
	if change.ID != id || change.To != "failed" || change.ExitCode == nil || *change.ExitCode != 7 || !strings.Contains(contextLabel(*m), "1 failed") {
		t.Fatal(change, contextLabel(*m))
	}
	child, _, err := r.runtimeContext(context.Background(), "child", r.selection, contextCursor{})
	if err != nil || len(decode(child).Changes) != 0 {
		t.Fatal(child, err)
	}
	m, _, err = r.runtimeContext(context.Background(), "main", r.selection, next)
	if err != nil || m != nil {
		t.Fatal(m, err)
	}
}

func TestAncestorInstructionsAreOrderedAndRefreshAtRequestBoundary(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	root := t.TempDir()
	cwd := filepath.Join(root, "project", "nested")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	r.Workspace.Root = cwd
	paths := []string{filepath.Join(root, "AGENTS.md"), filepath.Join(root, "project", "AGENTS.md"), filepath.Join(cwd, "AGENTS.md")}
	for i, path := range paths {
		if err := os.WriteFile(path, []byte(strings.Repeat("instruction ", i+1)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	deeper := filepath.Join(cwd, "deeper", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(deeper), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deeper, []byte("Only for deeper files."), 0600); err != nil {
		t.Fatal(err)
	}
	m, cursor, err := r.runtimeContext(context.Background(), "main", r.selection, contextCursor{})
	if err != nil {
		t.Fatal(err)
	}
	var result runtimeContext
	if err := json.Unmarshal([]byte(m.Content), &result); err != nil {
		t.Fatal(err)
	}
	if result.Project == nil || len(result.Project.Instructions) < len(paths) {
		t.Fatal("missing ancestor instructions", result.Project)
	}
	for _, instruction := range result.Project.Instructions {
		if instruction.Path == deeper {
			t.Fatal("deeper scoped instructions leaked into global context")
		}
	}
	// Machine-wide ancestors may also exist; the fixture is the final three.
	instructions := result.Project.Instructions[len(result.Project.Instructions)-len(paths):]
	for i, instruction := range instructions {
		if instruction.Path != paths[i] || instruction.Content != strings.Repeat("instruction ", i+1) {
			t.Fatal("incorrect ancestor precedence", instructions)
		}
	}
	if err := os.WriteFile(paths[1], []byte("updated parent instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	m, cursor, err = r.runtimeContext(context.Background(), "main", r.selection, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(m.Content), &result); err != nil {
		t.Fatal(err)
	}
	if result.Project == nil || result.Project.Instructions[len(result.Project.Instructions)-2].Content != "updated parent instructions" {
		t.Fatal("changed ancestor instructions were not supplied", result.Project)
	}
	if err := os.Remove(paths[1]); err != nil {
		t.Fatal(err)
	}
	m, _, err = r.runtimeContext(context.Background(), "main", r.selection, cursor)
	if err != nil {
		t.Fatal(err)
	}
	result = runtimeContext{}
	if err := json.Unmarshal([]byte(m.Content), &result); err != nil {
		t.Fatal(err)
	}
	if result.Project == nil {
		t.Fatal("removed instructions were not reported")
	}
	for _, instruction := range result.Project.Instructions {
		if instruction.Path == paths[1] {
			t.Fatal("removed parent instructions remained in context")
		}
	}
}

func TestReportedUsageSnapshotOwnershipAndModelBoundary(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	cached, reasoning := 0, 15
	source := &ReportedUsage{Model: "previous model", Tokens: provider.Usage{InputTokens: 100, OutputTokens: 20, CachedInputTokens: &cached, ReasoningOutputTokens: &reasoning}}
	r.reported = copyReported(source)
	cached = 99
	r.usage = ContextUsage{Model: "next model", Parts: []TokenPart{{Name: "Instructions", Tokens: 100}}}
	snapshot := r.UsageSnapshot()
	if snapshot.Model != "next model" || snapshot.Reported.Model != "previous model" || *snapshot.Reported.Tokens.CachedInputTokens != 0 {
		t.Fatal(snapshot)
	}
	*snapshot.Reported.Tokens.ReasoningOutputTokens = 99
	snapshot.Parts[0].Tokens = 0
	if got := r.UsageSnapshot(); *got.Reported.Tokens.ReasoningOutputTokens != 15 || got.Parts[0].Tokens != 100 {
		t.Fatal("snapshot aliases runtime")
	}
	if _, err := r.Command("/new"); err != nil {
		t.Fatal(err)
	}
	if r.UsageSnapshot().Reported != nil {
		t.Fatal("new session retained previous counters")
	}
}

func TestInterruptedNativeCompletionResumesCanonicalHistory(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	step := 0
	r.Provider = &childProvider{stream: func(_ context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
		step++
		if step == 1 {
			if err := emit(provider.StreamEvent{Kind: "text", Text: "Partial"}); err != nil {
				return err
			}
			if err := emit(provider.StreamEvent{Kind: "state", StateVersion: 1, StateItem: []byte(`{"type":"function_call","call_id":"c","name":"read","arguments":"{}"}`)}); err != nil {
				return err
			}
			return context.Canceled
		}
		for _, m := range req.Messages {
			if m.State != nil {
				t.Fatal("interrupted native state was replayed")
			}
		}
		return emit(provider.StreamEvent{Kind: "text", Text: "Resumed"})
	}}
	if err := r.Run(&provider.Message{Role: "user", Content: "Start"}); err != context.Canceled {
		t.Fatal(err)
	}
	if err := r.Run(&provider.Message{Role: "user", Content: "Continue"}); err != nil {
		t.Fatal(err)
	}
}

func TestRepeatingTimerContextHighlightsFiring(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Observe timer")
	// Model the supervisor's already-observed transition without a timing race.
	r.timers.mu.Lock()
	_, cancel := context.WithCancel(r.ctx)
	r.timers.items["timer"] = &wakeup{ID: "timer", Name: "Repeated check", Status: "scheduled", Repeat: 60, Fired: 2, cancel: cancel}
	vtimer := *r.timers.items["timer"]
	r.timers.mu.Unlock()
	r.publishTimer(vtimer)
	m, _, err := r.runtimeContext(context.Background(), "main", r.selection, contextCursor{timers: map[string]string{"timer": "scheduled/1"}})
	var v runtimeContext
	if err != nil || json.Unmarshal([]byte(m.Content), &v) != nil {
		t.Fatal(m, err)
	}
	if len(v.Changes) != 1 || v.Changes[0].To != "fired" || v.Changes[0].From != "scheduled" || v.Changes[0].FiredCount != 2 || len(v.Timers) != 1 {
		t.Fatal(v)
	}
	if !strings.Contains(contextLabel(*m), "1 fired") {
		t.Fatal(contextLabel(*m))
	}
}

func TestSuccessfulResponseWithoutUsageClearsOlderCounters(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	step := 0
	r.Provider = &childProvider{stream: func(_ context.Context, _ provider.Request, emit func(provider.StreamEvent) error) error {
		step++
		if err := emit(provider.StreamEvent{Kind: "text", Text: "Done"}); err != nil {
			return err
		}
		var usage *provider.Usage
		if step == 1 {
			usage = &provider.Usage{InputTokens: 100, OutputTokens: 10}
		}
		return emit(provider.StreamEvent{Kind: "completed", Usage: usage})
	}}
	for _, prompt := range []string{"First", "Second"} {
		if err := r.Run(&provider.Message{Role: "user", Content: prompt}); err != nil {
			t.Fatal(err)
		}
		if (r.UsageSnapshot().Reported != nil) != (prompt == "First") {
			t.Fatal("stale reported counters", prompt)
		}
	}
}

func TestStableInstructionsAndAppendOnlyContextAtToolBoundaries(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	var requests []provider.Request
	r.Provider = &childProvider{stream: func(_ context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
		requests = append(requests, req)
		if len(requests) == 1 {
			return emit(provider.StreamEvent{Kind: "call", Call: &provider.ToolCall{ID: "read", Name: "glob", Arguments: []byte(`{"pattern":"*.txt"}`)}})
		}
		return emit(provider.StreamEvent{Kind: "text", Text: "Done"})
	}}
	if err := r.Run(&provider.Message{Role: "user", Content: "Inspect"}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0].System != systemTemplate || requests[1].System != requests[0].System {
		t.Fatal("instructions changed")
	}
	for i, old := range requests[0].Messages {
		next := requests[1].Messages[i]
		if old.Role != next.Role || old.Content != next.Content {
			t.Fatal("rewrote previous request context", old, next)
		}
	}
	contexts := 0
	for _, message := range requests[1].Messages {
		if message.Role == "developer" && message.Runtime {
			contexts++
		}
	}
	if contexts != 1 {
		t.Fatal("unchanged tool boundary appended runtime context", contexts)
	}
	updates := 0
	for len(events) > 0 {
		event := <-events
		if event.Kind == "runtime_context" {
			updates++
			if event.EntryID == 0 {
				t.Fatal("empty context UI row", event)
			}
		}
	}
	if updates != 1 {
		t.Fatal("unchanged snapshot generated UI update", updates)
	}
}

func TestCompactionResuppliesUnchangedProjectContext(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.selection.Model.Budget.RecentTokensTarget = 200
	if err := os.WriteFile(filepath.Join(r.Workspace.Root, "AGENTS.md"), []byte("Retain units."), 0600); err != nil {
		t.Fatal(err)
	}
	coding := 0
	r.Provider = &childProvider{stream: func(_ context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
		if req.NoTools {
			if req.ConversationID != r.Current()+"/compaction" {
				t.Fatal("incorrect compaction identity", req.ConversationID)
			}
			return emit(provider.StreamEvent{Kind: "text", Text: "Continue with the retained task."})
		}
		coding++
		if req.ConversationID != r.Current() {
			t.Fatal("incorrect continuation identity", req.ConversationID)
		}
		if req.System != systemTemplate {
			t.Fatal("compaction changed coding instructions")
		}
		var v runtimeContext
		last := req.Messages[len(req.Messages)-1]
		if err := json.Unmarshal([]byte(last.Content), &v); err != nil || v.Project == nil {
			t.Fatal("missing project context", last, err)
		}
		return emit(provider.StreamEvent{Kind: "text", Text: "Done"})
	}}
	if err := r.Run(&provider.Message{Role: "user", Content: "Initial"}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{strings.Repeat("old research ", 700), "Recent task"} {
		if _, err := r.Store.Append(r.Current(), "", "main", "message", "user", true, provider.Message{Role: "user", Content: text}); err != nil {
			t.Fatal(err)
		}
	}
	before := r.Current()
	if _, err := r.Command("/compact retain units"); err != nil {
		t.Fatal(err)
	}
	if before == r.Current() {
		t.Fatal("no continuation")
	}
	if err := r.Run(&provider.Message{Role: "user", Content: "Continue"}); err != nil {
		t.Fatal(err)
	}
	if coding != 2 {
		t.Fatal(coding)
	}
}
