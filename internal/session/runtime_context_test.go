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

	"ttc/internal/capture"
	"ttc/internal/jobs"
	"ttc/internal/llm"
	"ttc/internal/skills"
	"ttc/internal/tool"
)

func TestRuntimeContextUpdateExplicitClearingAndIndependentProjectLists(t *testing.T) {
	formats := []nativeReadGroup{{Extensions: []string{".pdf"}, MaxBytes: 32}}
	emptyFormats := []nativeReadGroup{}
	previous := runtimeContext{
		Cwd: "/old", IsRepo: true, Branch: "main", Scratch: "/scratch", Date: "yesterday", Model: "old",
		ImageInput: true, ImageClick: true, BinaryFiles: &formats,
		Project: &projectContext{Instructions: []projectInstruction{{Path: "AGENTS.md", Content: "old"}}, Skills: []skills.Skill{{Name: "old"}}},
	}
	current := runtimeContext{BinaryFiles: &emptyFormats, Project: &projectContext{Instructions: []projectInstruction{}, Skills: []skills.Skill{}}}
	update := contextUpdate(&previous, &current)
	data, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	fields := runtimeFields(t, &llm.Message{Type: llm.TypeRuntimeContext, Content: string(data)}, "runtime_context_update")
	for _, key := range []string{"is_repo", "image_input", "image_click"} {
		if string(fields[key]) != "false" {
			t.Fatal("false clearing omitted", key, string(data))
		}
	}
	for _, key := range []string{"cwd", "branch", "scratch_directory", "date_utc", "model"} {
		if string(fields[key]) != `""` {
			t.Fatal("empty clearing omitted", key, string(data))
		}
	}
	if string(fields["binary_files"]) != "[]" {
		t.Fatal("capability clearing omitted", string(data))
	}
	var project map[string]json.RawMessage
	if err := json.Unmarshal(fields["project"], &project); err != nil {
		t.Fatal(err)
	}
	if string(project["instructions"]) != "[]" || string(project["available_skills"]) != "[]" {
		t.Fatal("project clearing omitted", string(data))
	}
	current.Project.Instructions = previous.Project.Instructions
	update = contextUpdate(&previous, &current)
	projectSet := update.Set["project"].(map[string]any)
	if len(projectSet) != 1 || projectSet["available_skills"] == nil {
		t.Fatal("skill-only change resent instructions", projectSet)
	}
	if unchanged := contextUpdate(&current, &current); len(unchanged.Set) != 0 {
		t.Fatal("unchanged state emitted replacements", unchanged)
	}
}

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

// runtimeFields decodes the wire envelope without applying baseline defaults to a
// delta. Tests must check key presence to distinguish omission from clearing.
func runtimeFields(t *testing.T, message *llm.Message, kind string) map[string]json.RawMessage {
	t.Helper()
	if message == nil || message.Role() != "developer" || message.Type != llm.TypeRuntimeContext {
		t.Fatalf("missing developer runtime message: %+v", message)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(message.Content), &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["type"]) != `"`+kind+`"` {
		t.Fatalf("expected %s: %s", kind, message.Content)
	}
	if kind == "runtime_context_update" {
		if len(fields) != 2 {
			t.Fatal("delta has fields outside type/set", message.Content)
		}
		raw := fields["set"]
		fields = nil
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
	}
	return fields
}

func TestRuntimeContextProjectChangesAndFinishedJobs(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Inspect runtime state")
	m, cursor, err := r.runtimeContext(context.Background(), "main", r.selection, contextCursor{})
	if err != nil {
		t.Fatal(err)
	}
	initial := runtimeFields(t, m, "runtime_context")
	for _, key := range []string{"project", "live_jobs", "live_timers", "children", "binary_files"} {
		if _, ok := initial[key]; !ok {
			t.Fatal("incomplete baseline", key)
		}
	}
	if _, ok := initial["changes_since_previous_request"]; ok {
		t.Fatal("baseline includes historical transitions")
	}
	m, cursor, err = r.runtimeContext(context.Background(), "main", r.selection, cursor)
	if err != nil || m != nil {
		t.Fatal(m, err)
	}
	if err = os.WriteFile(filepath.Join(r.Workspace.Root, "AGENTS.md"), []byte("Use precise units."), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := r.Jobs.Start("main", "exit 7", r.Workspace.Root, 0, true, false, false, capture.OutputOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Jobs.Wait(context.Background(), "main", id, nil); err != nil {
		t.Fatal(err)
	}
	m, next, err := r.runtimeContext(context.Background(), "main", r.selection, cursor)
	if err != nil {
		t.Fatal(err)
	}
	fields := runtimeFields(t, m, "runtime_context_update")
	var project map[string]json.RawMessage
	if err := json.Unmarshal(fields["project"], &project); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 || len(project) != 1 || project["instructions"] == nil {
		t.Fatal("instruction-only change resent unrelated state", m.Content)
	}
	child, _, err := r.runtimeContext(context.Background(), "main/child_fixture", r.selection, contextCursor{})
	if err != nil {
		t.Fatal(err)
	}
	fresh := runtimeFields(t, child, "runtime_context")
	if string(fresh["live_jobs"]) != "[]" || fresh["changes_since_previous_request"] != nil {
		t.Fatal("fresh child received terminal history", child.Content)
	}
	m, _, err = r.runtimeContext(context.Background(), "main", r.selection, next)
	if err != nil || m != nil {
		t.Fatal(m, err)
	}
}

func TestFreshRuntimeBaselineIncludesOnlyAccessibleActiveInventories(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Inspect fresh inventories")
	actor := "main/child_fixture"
	for _, job := range []jobs.Snapshot{
		{ID: "own", Owner: actor, Status: "running"},
		{ID: "descendant", Owner: actor + "/child_nested", Status: "running"},
		{ID: "sibling", Owner: "main/child_other", Status: "running"},
		{ID: "finished", Owner: actor, Status: "completed"},
	} {
		r.publishJob(job)
	}
	for _, timer := range []wakeup{
		{ID: "own", actor: actor, Status: "scheduled"},
		{ID: "descendant", actor: actor + "/child_nested", Status: "scheduled"},
		{ID: "sibling", actor: "main/child_other", Status: "scheduled"},
		{ID: "cancelled", actor: actor, Status: "cancelled"},
	} {
		r.publishTimer(timer)
	}
	if err := r.publishChild(tool.ChildView{ID: actor, ChildState: "running", AssignmentStatus: "running"}); err != nil {
		t.Fatal(err)
	}
	message, _, err := r.runtimeContext(context.Background(), actor, r.selection, contextCursor{})
	if err != nil {
		t.Fatal(err)
	}
	fields := runtimeFields(t, message, "runtime_context")
	var view runtimeContext
	if err := json.Unmarshal([]byte(message.Content), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Jobs) != 2 || view.Jobs[0].ID != "descendant" || view.Jobs[1].ID != "own" || len(view.Timers) != 2 || view.Timers[0].ID != "descendant" || view.Timers[1].ID != "own" || len(view.Children) != 1 || view.Children[0].ID != actor {
		t.Fatal("baseline did not reflect accessible active inventories", view)
	}
	if fields["changes_since_previous_request"] != nil {
		t.Fatal("fresh actor inherited terminal transitions", message.Content)
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
	fields := runtimeFields(t, m, "runtime_context_update")
	if err := json.Unmarshal(fields["project"], &result.Project); err != nil {
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
	fields = runtimeFields(t, m, "runtime_context_update")
	if err := json.Unmarshal(fields["project"], &result.Project); err != nil {
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
	source := &ReportedUsage{Model: "previous model", Tokens: llm.Usage{InputTokens: 100, OutputTokens: 20, CachedInputTokens: &cached, ReasoningOutputTokens: &reasoning}}
	r.reported = copyReported(source)
	cached = 99
	r.usage = ContextUsage{Model: "next model"}
	snapshot := r.UsageSnapshot()
	if snapshot.Model != "next model" || snapshot.Reported.Model != "previous model" || *snapshot.Reported.Tokens.CachedInputTokens != 0 {
		t.Fatal(snapshot)
	}
	*snapshot.Reported.Tokens.ReasoningOutputTokens = 99
	if got := r.UsageSnapshot(); *got.Reported.Tokens.ReasoningOutputTokens != 15 {
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
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		step++
		if step == 1 {
			if err := emit(llm.StreamEvent{Kind: "text", Text: "Partial"}); err != nil {
				return err
			}
			if err := emit(llm.StreamEvent{Kind: "state", StateVersion: 1, StateItem: []byte(`{"type":"function_call","call_id":"c","name":"read","arguments":"{}"}`)}); err != nil {
				return err
			}
			return context.Canceled
		}
		for _, m := range req.Messages {
			if m.State != nil {
				t.Fatal("interrupted native state was replayed")
			}
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Resumed"})
	}}
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Start"}); err != context.Canceled {
		t.Fatal(err)
	}
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Continue"}); err != nil {
		t.Fatal(err)
	}
}

func TestRepeatingTimerActivityDoesNotEmitContext(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Observe timer")
	_, cursor, err := r.runtimeContext(context.Background(), "main", r.selection, contextCursor{})
	if err != nil {
		t.Fatal(err)
	}
	r.publishTimer(wakeup{ID: "timer", actor: "main", Name: "Repeated check", Status: "scheduled", Repeat: 60, Fired: 2})
	m, _, err := r.runtimeContext(context.Background(), "main", r.selection, cursor)
	if err != nil || m != nil {
		t.Fatal("activity emitted context", m, err)
	}
	m, _, err = r.runtimeContext(context.Background(), "main", r.selection, contextCursor{})
	if err != nil {
		t.Fatal(err)
	}
	fields := runtimeFields(t, m, "runtime_context")
	var timers []TimerView
	if err := json.Unmarshal(fields["live_timers"], &timers); err != nil || len(timers) != 1 || timers[0].ID != "timer" {
		t.Fatal("fresh baseline omitted active timer", timers, err)
	}
}

func TestSuccessfulResponseWithoutUsageClearsOlderCounters(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	step := 0
	r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		step++
		if err := emit(llm.StreamEvent{Kind: "text", Text: "Done"}); err != nil {
			return err
		}
		var usage *llm.Usage
		if step == 1 {
			usage = &llm.Usage{InputTokens: 100, OutputTokens: 10}
		}
		return emit(llm.StreamEvent{Kind: "completed", Usage: usage})
	}}
	for _, prompt := range []string{"First", "Second"} {
		if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: prompt}); err != nil {
			t.Fatal(err)
		}
		if (r.UsageSnapshot().Reported != nil) != (prompt == "First") {
			t.Fatal("stale reported counters", prompt)
		}
	}
}

func TestStableInstructionsAndAppendOnlyContextAtToolBoundaries(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	var requests []llm.Request
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		requests = append(requests, req)
		if len(requests) == 1 {
			return emit(llm.StreamEvent{Kind: "call", Call: &llm.ToolCall{ID: "read", Name: "glob", Arguments: []byte(`{"pattern":"*.txt"}`)}})
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Done"})
	}}
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Inspect"}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0].System != systemTemplate || requests[1].System != requests[0].System {
		t.Fatal("instructions changed")
	}
	for i, old := range requests[0].Messages {
		next := requests[1].Messages[i]
		if old.Type != next.Type || old.Content != next.Content {
			t.Fatal("rewrote previous request context", old, next)
		}
	}
	contexts := 0
	for _, message := range requests[1].Messages {
		if message.Role() == "developer" && message.Type.IsRuntime() {
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
	r.selection.Model.Budget.RecentTokensMin = 0
	r.selection.Model.Budget.RecentTokensMax = 200
	if err := os.WriteFile(filepath.Join(r.Workspace.Root, "AGENTS.md"), []byte("Retain units."), 0600); err != nil {
		t.Fatal(err)
	}
	coding := 0
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		if req.NoTools {
			if req.ConversationID != r.Current() {
				t.Fatal("incorrect compaction identity", req.ConversationID)
			}
			return emit(llm.StreamEvent{Kind: "text", Text: "Continue with the retained task."})
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
		return emit(llm.StreamEvent{Kind: "text", Text: "Done"})
	}}
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Initial"}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{strings.Repeat("old research ", 700), "Recent task"} {
		if _, err := r.Store.Append(r.Current(), "", "main", "message", "user", true, llm.Message{Type: llm.TypeUserInput, Content: text}); err != nil {
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
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Continue"}); err != nil {
		t.Fatal(err)
	}
	if coding != 2 {
		t.Fatal(coding)
	}
}
