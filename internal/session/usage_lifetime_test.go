package session

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"

	"ttc/internal/provider"
	"ttc/internal/tool"
)

func lifetimeUsage() provider.Usage {
	cached, written, reasoning := 40, 10, 5
	return provider.Usage{InputTokens: 100, OutputTokens: 20, CachedInputTokens: &cached, CacheWriteTokens: &written, ReasoningOutputTokens: &reasoning}
}

func assertLifetimeTotals(t *testing.T, r *Runtime, requests int) {
	t.Helper()
	u := lifetimeUsage()
	u.InputTokens *= requests
	u.OutputTokens *= requests
	*u.CachedInputTokens *= requests
	*u.CacheWriteTokens *= requests
	*u.ReasoningOutputTokens *= requests
	want := UsageTotals{Requests: requests, ReportedRequests: requests, Tokens: u}
	if got := r.UsageSnapshot().Totals; !reflect.DeepEqual(got, want) {
		t.Fatalf("run totals = %+v (%+v), want %+v (%+v)", got, got.Tokens, want, want.Tokens)
	}
}

func TestUsageLifetimeAcrossSessionChangesWithChild(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	parentCalls := 0
	r.Provider = &childProvider{stream: func(_ context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
		if !strings.Contains(req.System, "You are an isolated child agent.") {
			parentCalls++
			if parentCalls == 1 {
				call := provider.ToolCall{ID: "child", Name: "subagent", Arguments: []byte(`{"prompt":"audit","label":"audit","persistent":false}`)}
				if err := emit(provider.StreamEvent{Kind: "call", Call: &call}); err != nil {
					return err
				}
			}
		}
		if err := emit(provider.StreamEvent{Kind: "text", Text: "Audit complete."}); err != nil {
			return err
		}
		u := lifetimeUsage()
		return emit(provider.StreamEvent{Kind: "completed", Usage: &u})
	}}
	if err := r.Run(&provider.Message{Role: "user", Content: "Run an audit"}); err != nil {
		t.Fatal(err)
	}
	assertLifetimeTotals(t, r, 3) // Parent tool request, child answer, parent answer.
	if got := r.UsageSnapshot().Reported; got == nil || !reflect.DeepEqual(got.Tokens, lifetimeUsage()) {
		t.Fatal("parent context usage included child/run totals", got)
	}
	saved := r.Current()
	for _, command := range []string{"/branch 0", "/redo", "/undo", "/redo", "/load " + saved, "/clear", "/load " + saved} {
		if _, err := r.Command(command); err != nil {
			t.Fatal(command, err)
		}
		assertLifetimeTotals(t, r, 3)
		if got := r.UsageSnapshot(); got.Reported != nil || got.RequestID != 0 || got.Input != 0 {
			t.Fatal(command, "retained parent context counters", got)
		}
	}
	if _, err := r.Command("/load missing-session"); err == nil {
		t.Fatal("invalid session loaded")
	}
	assertLifetimeTotals(t, r, 3)
	if err := r.Run(&provider.Message{Role: "user", Content: "Continue after loading"}); err != nil {
		t.Fatal(err)
	}
	assertLifetimeTotals(t, r, 4)
	if _, err := r.Command("/new"); err != nil {
		t.Fatal(err)
	}
	if got := r.UsageSnapshot().Totals; !reflect.DeepEqual(got, UsageTotals{}) {
		t.Fatal("/new retained run totals", got)
	}
	if _, err := r.Command("/load " + saved); err != nil {
		t.Fatal(err)
	}
	if got := r.UsageSnapshot().Totals; !reflect.DeepEqual(got, UsageTotals{}) {
		t.Fatal("loading history rebuilt totals after /new", got)
	}
	if err := r.Run(&provider.Message{Role: "user", Content: "Start counting again"}); err != nil {
		t.Fatal(err)
	}
	assertLifetimeTotals(t, r, 1)
}

func TestUsageLifetimeRestartDoesNotRestoreHistory(t *testing.T) {
	r, _ := runtimeFixture(t, []provider.ScriptResponse{{Text: "Durable answer"}})
	r.Emit = nil
	if err := r.Run(&provider.Message{Role: "user", Content: "Save a response"}); err != nil {
		t.Fatal(err)
	}
	if r.UsageSnapshot().Totals.ReportedRequests != 1 {
		t.Fatal("missing usage before restart")
	}
	saved, selection := r.Current(), r.CurrentSelection()
	r.Close()
	restarted := New(context.Background(), r.Store, r.Workspace, r.Provider, selection, saved, r.Skills, tool.WebSearchConfig{}, nil)
	t.Cleanup(restarted.Close)
	if got := restarted.UsageSnapshot(); !reflect.DeepEqual(got.Totals, UsageTotals{}) || got.Reported != nil {
		t.Fatal("restart rebuilt counters from durable requests", got)
	}
	if entries, err := restarted.Entries(); err != nil || len(entries) == 0 {
		t.Fatal("restart lost history rather than ignoring its usage", entries, err)
	}
	if _, err := restarted.Command("/load " + saved); err != nil {
		t.Fatal(err)
	}
	if got := restarted.UsageSnapshot().Totals; !reflect.DeepEqual(got, UsageTotals{}) {
		t.Fatal("load rebuilt counters after restart", got)
	}
}

func TestUsageLifetimeCompactionPreservesEveryCounter(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		name := "manual"
		if automatic {
			name = "automatic"
		}
		t.Run(name, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			compactionBudget(t, r)
			seedCompactionHistory(t, r, strings.Repeat("Earlier research notes. ", 700))
			if !automatic {
				if _, err := r.Store.Append(r.Current(), "", "main", "message", "user", true, provider.Message{Role: "user", Content: "Continue"}); err != nil {
					t.Fatal(err)
				}
			}
			u := lifetimeUsage()
			r.recordUsage(&u)
			before := r.Current()
			r.Provider = &childProvider{stream: func(_ context.Context, _ provider.Request, emit func(provider.StreamEvent) error) error {
				if err := emit(provider.StreamEvent{Kind: "text", Text: "Earlier research completed; continue the task."}); err != nil {
					return err
				}
				u := lifetimeUsage()
				return emit(provider.StreamEvent{Kind: "completed", Usage: &u})
			}}
			requests := 2
			if automatic {
				requests++ // Summary plus the coding response after handoff.
				if err := r.Run(&provider.Message{Role: "user", Content: "Continue"}); err != nil {
					t.Fatal(err)
				}
			} else if _, err := r.Command("/compact"); err != nil {
				t.Fatal(err)
			}
			if r.Current() == before {
				t.Fatal("test did not compact")
			}
			assertLifetimeTotals(t, r, requests)
		})
	}
}

func TestCompactionUsageCountsOnlyAttemptedInferenceDespitePersistenceFailure(t *testing.T) {
	for _, test := range []struct {
		name, trigger string
		calls         int
	}{
		{"system prompt", `CREATE TRIGGER fail_compaction BEFORE INSERT ON entries WHEN json_extract(NEW.content_json,'$.type')='system_prompt' BEGIN SELECT RAISE(ABORT, 'fixture compaction persistence failure'); END`, 0},
		{"input", `CREATE TRIGGER fail_compaction BEFORE INSERT ON entries WHEN json_extract(NEW.content_json,'$.type')='request_message' AND json_extract(NEW.content_json,'$.role')='user' BEGIN SELECT RAISE(ABORT, 'fixture compaction persistence failure'); END`, 0},
		{"reply", `CREATE TRIGGER fail_compaction BEFORE INSERT ON entries WHEN json_extract(NEW.content_json,'$.type')='request_message' AND json_extract(NEW.content_json,'$.role')='assistant' BEGIN SELECT RAISE(ABORT, 'fixture compaction persistence failure'); END`, 1},
		{"handoff", `CREATE TRIGGER fail_compaction BEFORE INSERT ON sessions WHEN NEW.predecessor_id IS NOT NULL BEGIN SELECT RAISE(ABORT, 'fixture compaction persistence failure'); END`, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			compactionBudget(t, r)
			seedCompactionHistory(t, r, "Earlier research")
			if _, err := r.Store.Append(r.Current(), "", "main", "message", "user", true, provider.Message{Role: "user", Content: "Continue"}); err != nil {
				t.Fatal(err)
			}
			before := r.Current()
			u := lifetimeUsage()
			r.recordUsage(&u)
			calls := 0
			r.Provider = &childProvider{stream: func(_ context.Context, _ provider.Request, emit func(provider.StreamEvent) error) error {
				calls++
				if err := emit(provider.StreamEvent{Kind: "text", Text: "Earlier research completed."}); err != nil {
					return err
				}
				u := lifetimeUsage()
				return emit(provider.StreamEvent{Kind: "completed", Usage: &u})
			}}
			if _, err := r.Store.DB.Exec(test.trigger); err != nil {
				t.Fatal(err)
			}
			_, err := r.compactContext(context.Background(), "", r.CurrentSelection())
			if err == nil || !strings.Contains(err.Error(), "fixture compaction persistence failure") || calls != test.calls || r.Current() != before {
				t.Fatal("wrong failure boundary or provider attempts", err, calls)
			}
			assertLifetimeTotals(t, r, 1+test.calls)
		})
	}
}

func TestUsageLifetimeConcurrentRecordingAndSnapshots(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	const workers, responses = 8, 50
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range responses {
				u := lifetimeUsage()
				r.recordUsage(&u)
				snapshot := r.UsageSnapshot()
				if snapshot.Totals.Tokens.CacheWriteTokens != nil {
					*snapshot.Totals.Tokens.CacheWriteTokens = -1
				}
			}
		})
	}
	wg.Wait()
	assertLifetimeTotals(t, r, workers*responses)
}
