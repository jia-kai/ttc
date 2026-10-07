package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ttc/internal/llm"
)

func TestBTWParallelFrozenPrefixReadOnlyAndCounters(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	if err := os.WriteFile(filepath.Join(r.Workspace.Root, "fixture.txt"), []byte("evidence\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mainReady, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var parentCalls, asideCalls atomic.Int32
	var parent llm.Request
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		cached, written, reasoning := 600, 100, 2
		finish := func() error {
			return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: 1000, OutputTokens: 10, CachedInputTokens: &cached, CacheWriteTokens: &written, ReasoningOutputTokens: &reasoning}})
		}
		if !strings.Contains(req.ConversationID, "/btw_") {
			if parentCalls.Add(1) == 1 {
				call := llm.ToolCall{ID: "main-read", Name: "read", Arguments: []byte(`{"path":"fixture.txt"}`)}
				if err := emit(llm.StreamEvent{Kind: "call", Call: &call}); err != nil {
					return err
				}
				return finish()
			}
			parent = req
			close(mainReady)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			if err := emit(llm.StreamEvent{Kind: "text", Text: "main finished"}); err != nil {
				return err
			}
			return finish()
		}
		if req.System != parent.System || req.Selection.Model.ID != parent.Selection.Model.ID {
			return errors.New("aside changed shared instructions or model")
		}
		for _, d := range req.Tools {
			if !readOnlyTool(d.Name) {
				return errors.New("mutating tool advertised")
			}
		}
		if asideCalls.Add(1) == 1 {
			if len(req.Messages) < len(parent.Messages)+3 {
				return errors.New("missing shared prefix")
			}
			for i := range parent.Messages {
				a, _ := json.Marshal(parent.Messages[i])
				b, _ := json.Marshal(req.Messages[i])
				if string(a) != string(b) {
					return errors.New("main prefix changed")
				}
			}
			if req.Messages[len(parent.Messages)].Content != btwInstruction {
				return errors.New("missing turn-local aside instruction")
			}
			for _, call := range []llm.ToolCall{
				{ID: "write", Name: "write", Arguments: []byte(`{"path":"forbidden.txt","content":"bad"}`)},
				{ID: "shell", Name: "shell", Arguments: []byte(`{"command":"touch forbidden-shell.txt"}`)},
				{ID: "question", Name: "question", Arguments: []byte(`{"questions":[]}`)},
				{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"bad","label":"bad"}`)},
				{ID: "read", Name: "read", Arguments: []byte(`{"path":"fixture.txt"}`)},
			} {
				if err := emit(llm.StreamEvent{Kind: "call", Call: &call}); err != nil {
					return err
				}
			}
			return finish()
		}
		failures := 0
		for _, m := range req.Messages {
			if m.Role == "tool" && strings.Contains(m.Content, "unknown_tool") {
				failures++
			}
		}
		if failures != 4 {
			return errors.New("forbidden dispatch did not return four failures")
		}
		if err := emit(llm.StreamEvent{Kind: "text", Text: "## Aside answer\n\nVerified evidence."}); err != nil {
			return err
		}
		return finish()
	}}
	done := make(chan error, 1)
	go func() { m := llm.Message{Role: "user", Content: "Keep main working"}; done <- r.Run(&m) }()
	select {
	case <-mainReady:
	case <-time.After(3 * time.Second):
		t.Fatal("main not ready")
	}
	id, err := r.StartBTW("What is in fixture.txt?")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	v, err := r.Jobs.Wait(ctx, "main", id, nil)
	if err != nil || v.Status != "completed" || !strings.Contains(v.Stdout, "Verified evidence") {
		t.Fatal(v, err)
	}
	for {
		select {
		case event := <-events:
			if event.Kind != "btw_result" {
				continue
			}
			entry, err := r.Store.Entry(event.EntryID)
			if err != nil {
				t.Fatal(err)
			}
			detail, err := r.Store.Inspect(entry)
			if err != nil || !strings.Contains(detail, "## Aside answer") {
				t.Fatal(detail, err)
			}
			goto inspected
		case <-ctx.Done():
			t.Fatal("missing aside popup")
		}
	}
inspected:
	select {
	case err := <-done:
		t.Fatal("aside interrupted main", err)
	default:
	}
	for _, path := range []string{"forbidden.txt", "forbidden-shell.txt"} {
		if _, err := os.Stat(filepath.Join(r.Workspace.Root, path)); !os.IsNotExist(err) {
			t.Fatal("aside mutated files", err)
		}
	}
	if r.HasNotifications() || r.PendingQuestion() != nil {
		t.Fatal("aside steered main or opened a question")
	}
	messages, err := r.Store.Messages(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if strings.Contains(m.Content, "Verified evidence") || m.Content == btwInstruction {
			t.Fatal("aside leaked into parent context")
		}
	}
	u := r.UsageSnapshot()
	if u.Totals.Requests != 3 || u.Totals.Tokens.InputTokens != 3000 || *u.Totals.Tokens.CachedInputTokens != 1800 || u.Reported.Tokens.InputTokens != 1000 {
		t.Fatal(u)
	}
	r.Interrupt()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestBTWAdmissionAndCancellation(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	if _, err := r.StartBTW("question"); err == nil {
		t.Fatal("empty session persisted by aside")
	}
	seedRuntime(t, r, "context")
	r.Provider = &childProvider{stream: func(ctx context.Context, _ llm.Request, _ func(llm.StreamEvent) error) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	for _, q := range []string{"", strings.Repeat("x", 4097), "bad\x00question"} {
		if _, err := r.StartBTW(q); err == nil {
			t.Fatal("accepted invalid question")
		}
	}
	for range 4 {
		if _, err := r.StartBTW("wait"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.StartBTW("fifth"); err == nil {
		t.Fatal("capacity exceeded")
	}
	if _, err := r.Command("/new"); err != nil {
		t.Fatal(err)
	}
	if len(r.Jobs.Live()) != 0 || r.UsageSnapshot().Totals.Requests != 0 {
		t.Fatal("reset retained aside jobs or counters")
	}
}

func TestUsageTotalsAccumulateAndSnapshotDoesNotAlias(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	cached, written, reasoning := 500, 100, 8
	u := llm.Usage{InputTokens: 1000, OutputTokens: 10, CachedInputTokens: &cached, CacheWriteTokens: &written, ReasoningOutputTokens: &reasoning}
	r.recordUsage(&u)
	r.recordUsage(&u)
	snapshot := r.UsageSnapshot()
	if snapshot.Totals.Requests != 2 || snapshot.Totals.Tokens.InputTokens != 2000 || *snapshot.Totals.Tokens.CachedInputTokens != 1000 || *snapshot.Totals.Tokens.CacheWriteTokens != 200 || *snapshot.Totals.Tokens.ReasoningOutputTokens != 16 {
		t.Fatal(snapshot)
	}
	*snapshot.Totals.Tokens.CachedInputTokens = 99
	cached = 99
	if *r.UsageSnapshot().Totals.Tokens.CachedInputTokens != 1000 {
		t.Fatal("aliased counters")
	}
	r.recordUsage(nil)
	r.recordUsage(&llm.Usage{InputTokens: 200, OutputTokens: 2})
	snapshot = r.UsageSnapshot()
	if snapshot.Totals.ReportedRequests != 3 || snapshot.Totals.Requests != 4 || snapshot.Totals.Tokens.CachedInputTokens != nil {
		t.Fatal("missing counters presented as zero", snapshot)
	}
}

func TestBTWRejectsReadOnlyAndPendingRedo(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "question context")
	if _, err := r.Store.DB.Exec("UPDATE sessions SET read_only=1 WHERE id=?", r.Current()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.StartBTW("question"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatal(err)
	}
	if _, err := r.Store.DB.Exec("UPDATE sessions SET read_only=0 WHERE id=?", r.Current()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Command("/undo"); err != nil {
		t.Fatal(err)
	}
	before, err := r.Store.Session(r.Current())
	if err != nil || before.RedoTip == 0 {
		t.Fatal(before, err)
	}
	if _, err := r.StartBTW("question"); err == nil || !strings.Contains(err.Error(), "redo") {
		t.Fatal(err)
	}
	after, err := r.Store.Session(r.Current())
	if err != nil || before.RedoTip != after.RedoTip || len(r.Jobs.Live()) != 0 {
		t.Fatal("aside changed redo or launched work", after, err)
	}
}
