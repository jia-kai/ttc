package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
)

func compactionBudget(t *testing.T, r *Runtime) {
	t.Helper()
	message, _, err := r.runtimeContext(context.Background(), "main", r.selection, contextCursor{})
	if err != nil {
		t.Fatal(err)
	}
	u := estimateUsage(r.selection, systemTemplate, r.Tools.Definitions(), []llm.Message{*message})
	b := &r.selection.Model.Budget
	b.ContextLimit = u.Input + u.Reserved + 2000
	b.RecentTokensMin = 0
	b.RecentTokensMax = 700
	b.NextTurnInputReserve = 512
	b.SummaryOutputAllowance = 512
}

// seedCompactionHistory puts large completed model output in the prefix, rather
// than a human instruction that the bounded-input retention policy must copy.
func seedCompactionHistory(t *testing.T, r *Runtime, content string) {
	t.Helper()
	seedRuntime(t, r, "Earlier research task")
	if _, err := r.Store.Append(r.Current(), "", "main", "message", "assistant", true, llm.Message{Role: "assistant", Content: content}); err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticCompactionPreservesLiveStateAndContinuesBeforeRequest(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	compactionBudget(t, r)
	seedCompactionHistory(t, r, strings.Repeat("Previous research notes. ", 700))
	before, generation := r.Current(), r.Generation()
	job, err := r.Jobs.Start("main", "sleep 30", r.Workspace.Root, 0, true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	summaries, coding := 0, 0
	var childTail int64
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		if req.NoTools {
			summaries++
			if !strings.Contains(req.Messages[0].Content, "Previous research notes.") {
				t.Fatal("summary omitted prefix")
			}
			r.routeMu.RLock()
			childTail, err = r.Store.Append(r.Current(), "", "main/child", "status", "", false, map[string]string{"type": "child_progress", "text": "Background evidence committed during summarization."})
			r.routeMu.RUnlock()
			if err != nil {
				return err
			}
			if err := emit(llm.StreamEvent{Kind: "text", Text: "Earlier research completed; continue the current task."}); err != nil {
				return err
			}
		} else {
			coding++
			if r.Current() == before || !strings.Contains(req.Messages[0].Content, "Earlier research completed") {
				t.Fatal("coding request started before handoff", req.Messages)
			}
			if !contextbuild.Fits(req.Selection, req.System, req.Tools, req.Messages, false) {
				t.Fatal("oversized coding request sent")
			}
			if err := emit(llm.StreamEvent{Kind: "text", Text: "Continued successfully."}); err != nil {
				return err
			}
		}
		return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: 100, OutputTokens: 10}})
	}}
	if err := r.Run(&llm.Message{Role: "user", Content: "Continue the research."}); err != nil {
		t.Fatal(err)
	}
	if summaries != 1 || coding != 1 || r.Generation() != generation {
		t.Fatal(summaries, coding, r.Generation())
	}
	old, err := r.Store.Session(before)
	if err != nil || !old.ReadOnly {
		t.Fatal(old, err)
	}
	live := r.Jobs.Live()
	if len(live) != 1 || live[0].ID != job {
		t.Fatal("compaction lost live job", live)
	}
	if totals := r.UsageSnapshot().Totals; totals.Requests != 2 || totals.Tokens.InputTokens != 200 {
		t.Fatal("summary usage was not accumulated", totals)
	}
	continuations := 0
	for len(events) > 0 {
		if (<-events).Kind == "continuation" {
			continuations++
		}
	}
	if continuations != 1 {
		t.Fatal("expected one UI continuation reload", continuations)
	}
	entries, err := r.Store.Branch(r.Current(), 0)
	if err != nil {
		t.Fatal(err)
	}
	foundTail := false
	for _, entry := range entries {
		if entry.Source == childTail {
			foundTail = true
		}
	}
	if !foundTail {
		t.Fatal("committed child tail was lost during handoff")
	}
}

func TestAutomaticCompactionAtToolBoundaryKeepsRecentModelsAndUndoBaseline(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	const prompt = "Edit the result and verify it."
	before := r.Current()
	step, summaries := 0, 0
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		if req.NoTools {
			summaries++
			return emit(llm.StreamEvent{Kind: "text", Text: "The original request is ongoing; earlier file edits are recorded in the archive."})
		}
		step++
		if step == 4 {
			if r.Current() == before || summaries != 1 {
				t.Fatal("tool boundary did not compact")
			}
			found := map[string]bool{}
			pending := map[string]bool{}
			for _, message := range req.Messages {
				if message.State != nil {
					t.Fatal("opaque replay survived compaction")
				}
				found[message.Content] = true
				if message.Role == "assistant" && strings.HasPrefix(message.Content, "Model reasoning 1") {
					t.Fatal("older model message was not compacted")
				}
				for _, call := range message.Calls {
					pending[call.ID] = true
				}
				if message.Role == "tool" {
					if !pending[message.CallID] {
						t.Fatal("orphan retained tool result", message)
					}
					delete(pending, message.CallID)
				}
			}
			if !found[prompt] || !found["Model reasoning 2"] || !found["Model reasoning 3"] || found["Model reasoning 1"] || len(pending) != 0 {
				t.Fatal("incorrect retained recent cycles", found, pending)
			}
		}
		if step == 5 {
			return emit(llm.StreamEvent{Kind: "text", Text: "Verified after continuation."})
		}
		text := fmt.Sprintf("Model reasoning %d", step)
		if step == 1 {
			text += "\n" + strings.Repeat("older analysis ", 600)
		}
		if err := emit(llm.StreamEvent{Kind: "text", Text: text}); err != nil {
			return err
		}
		calls := []llm.ToolCall{}
		if step == 3 {
			for _, id := range []string{"read-a", "read-b"} {
				calls = append(calls, llm.ToolCall{ID: id, Name: "read", Arguments: []byte(`{"path":"result.txt"}`)})
			}
			// Switching to a tighter catalog model at this boundary exercises
			// compaction of an active turn without first ending that turn.
			next := req.Selection
			next.Model.ID = "smaller-context"
			u := estimateUsage(req.Selection, req.System, req.Tools, req.Messages)
			next.Model.Budget.ContextLimit = u.Input + u.Reserved + 100
			next.Model.Budget.RecentTokensMin = 0
			next.Model.Budget.RecentTokensMax = 700
			next.Model.Budget.NextTurnInputReserve = 1
			next.Model.Budget.SummaryOutputAllowance = 512
			if err := r.RequestModel(next); err != nil {
				return err
			}
		} else {
			content := fmt.Sprintf("version %d\n", step)
			args, _ := json.Marshal(map[string]any{"path": "result.txt", "content": content})
			calls = append(calls, llm.ToolCall{ID: fmt.Sprintf("write-%d", step), Name: "write", Arguments: args})
		}
		for _, call := range calls {
			if err := emit(llm.StreamEvent{Kind: "call", Call: &call}); err != nil {
				return err
			}
		}
		return nil
	}}
	if err := r.Run(&llm.Message{Role: "user", Content: prompt}); err != nil {
		t.Fatal(err)
	}
	if step != 5 || summaries != 1 {
		t.Fatal(step, summaries)
	}
	continuation := r.Current()
	var sourcePrompt, prefixTip int64
	if err := r.Store.DB.QueryRow(`SELECT id FROM entries WHERE session_id=? AND role='user' AND model_visible=1 ORDER BY id LIMIT 1`, before).Scan(&sourcePrompt); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.DB.QueryRow(`SELECT id FROM file_changes WHERE session_id=? ORDER BY id LIMIT 1`, before).Scan(&prefixTip); err != nil {
		t.Fatal(err)
	}
	entries, err := r.Store.Branch(continuation, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundPrompt := false
	for _, entry := range entries {
		if entry.Source == sourcePrompt {
			foundPrompt = true
			if entry.FileTip != prefixTip {
				t.Fatal("separately retained prompt was not rebased to summarized edits", entry.FileTip, prefixTip)
			}
		}
	}
	if !foundPrompt {
		t.Fatal("original instruction was not copied separately")
	}
	if _, err := r.Command("/undo"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(r.Workspace.Root, "result.txt")); err != nil || string(data) != "version 1\n" {
		t.Fatal("undo crossed the compaction baseline", string(data), err)
	}
	if _, err := r.Command("/redo"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(r.Workspace.Root, "result.txt")); err != nil || string(data) != "version 4\n" {
		t.Fatal("redo lost continuation writes", string(data), err)
	}
}

func TestCompactionRetainsFittingRecentCyclesAndSummarizesEarlierHistory(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		for _, cycles := range []int{0, 1, 2} {
			t.Run(fmt.Sprintf("automatic=%v/cycles=%d", automatic, cycles), func(t *testing.T) {
				r, _ := runtimeFixture(t, nil)
				compactionBudget(t, r)
				seedCompactionHistory(t, r, strings.Repeat("Earlier completed research. ", 700))
				before := r.Current()
				newest := []llm.Message{{Role: "user", Content: "Continue with the sidebar widget."}}
				for i := range cycles {
					id := fmt.Sprintf("read-%d", i)
					newest = append(newest,
						llm.Message{Role: "developer", Runtime: true, Content: "Current runtime metadata"},
						llm.Message{Role: "assistant", Calls: []llm.ToolCall{{ID: id, Name: "read", Arguments: []byte(`{"path":"fixture.go"}`)}}},
						llm.Message{Role: "tool", CallID: id, Content: strings.Repeat("Recent tool result. ", 20)})
				}
				for _, message := range newest {
					if _, err := r.Store.Append(before, "", "main", "message", message.Role, true, message); err != nil {
						t.Fatal(err)
					}
				}
				summaries, coding := 0, 0
				r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
					if req.NoTools {
						summaries++
						if !strings.Contains(req.Messages[0].Content, "Earlier completed research.") {
							t.Fatal("summary omitted the older prefix")
						}
						return emit(llm.StreamEvent{Kind: "text", Text: "Earlier research completed."})
					}
					coding++
					if r.Current() == before || !contextbuild.Fits(req.Selection, req.System, req.Tools, req.Messages, false) {
						t.Fatal("coding request did not follow a fitting handoff")
					}
					return emit(llm.StreamEvent{Kind: "text", Text: "Continued."})
				}}
				var err error
				if automatic {
					err = r.Run(nil)
				} else {
					_, err = r.Command("/compact")
				}
				if err != nil || summaries != 1 || coding != map[bool]int{false: 0, true: 1}[automatic] || r.Current() == before {
					t.Fatal("compaction failed or retried", err, summaries, coding)
				}
				retained := newest[:1]
				if cycles > 0 {
					retained = append(append([]llm.Message(nil), retained...), newest[2:]...)
				}
				messages, err := r.Store.Messages(r.Current())
				if err != nil || len(messages) < len(retained)+1 {
					t.Fatal("retained history missing", err)
				}
				start := -1
				for i, message := range messages {
					if message.Role == "user" && message.Content == newest[0].Content {
						start = i
						break
					}
				}
				if start < 0 || start+len(retained) > len(messages) {
					t.Fatal("newest input and recent cycles missing")
				}
				copied := append([]llm.Message(nil), messages[start:start+len(retained)]...)
				for i := range copied {
					copied[i].InputSource, copied[i].InputTimeMS = "", 0
				}
				got, _ := json.Marshal(copied)
				want, _ := json.Marshal(retained)
				if string(got) != string(want) || r.checkContext() != nil {
					t.Fatal("newest turn changed or context was invalidated", string(got))
				}
			})
		}
	}
}

func TestAutomaticCompactionFailuresPreserveWritablePredecessor(t *testing.T) {
	for _, reason := range []string{"summary", "oversized", "cancel", "commit", "tail"} {
		t.Run(reason, func(t *testing.T) {
			r, events := runtimeFixture(t, nil)
			compactionBudget(t, r)
			seedCompactionHistory(t, r, strings.Repeat("Previous research notes. ", 700))
			before := r.Current()
			attempts := 0
			r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				attempts++
				if !req.NoTools {
					t.Fatal("coding request sent after failed compaction")
				}
				switch reason {
				case "summary":
					return errors.New("fixture summary failed")
				case "oversized":
					return emit(llm.StreamEvent{Kind: "text", Text: strings.Repeat("huge ", 30000)})
				case "cancel":
					r.Interrupt()
					return ctx.Err()
				case "commit":
					_, err := r.Store.DB.Exec(`CREATE TRIGGER fail_continuation BEFORE INSERT ON sessions WHEN NEW.predecessor_id IS NOT NULL BEGIN SELECT RAISE(ABORT, 'fixture commit failure'); END`)
					if err != nil {
						t.Fatal(err)
					}
				case "tail":
					r.routeMu.RLock()
					_, err := r.Store.Append(before, "", "main", "message", "user", true, llm.Message{Role: "user", Content: strings.Repeat("new committed tail ", 10000)})
					r.routeMu.RUnlock()
					if err != nil {
						t.Fatal(err)
					}
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Short valid summary."})
			}}
			err := r.Run(&llm.Message{Role: "user", Content: "Continue the research."})
			if err == nil || attempts != 1 || r.Current() != before {
				t.Fatal("failed compaction changed session or retried indefinitely", err, attempts)
			}
			if reason == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("interruption was lost", err)
			}
			old, err := r.Store.Session(before)
			if err != nil || old.ReadOnly {
				t.Fatal("failed handoff froze predecessor", old, err)
			}
			for len(events) > 0 {
				if event := <-events; event.Kind == "continuation" {
					t.Fatal("failed compaction emitted a UI reload")
				}
			}
		})
	}
}
