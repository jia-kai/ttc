package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// A zero next-turn reserve is valid. The mandatory recovery warning must be
// counted explicitly, not restored after admitting a summary that only fits
// without it.
func TestRecoveryCompactionCountsPendingWarningBeforeFit(t *testing.T) {
	for _, actor := range []string{"main", "main/child_budget", "main/btw_budget"} {
		for _, fits := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fits=%v", actor, fits), func(t *testing.T) {
				r, _ := runtimeFixture(t, nil)
				compactionBudget(t, r)
				seedRuntime(t, r, "Continue research")
				before := r.Current()
				aside := strings.Contains(actor, "/btw_")
				var turn string
				var err error
				if actor == "main" {
					turn, err = r.Store.BeginTurn(before, "user", r.selection)
				} else {
					turn, err = r.Store.BeginChildTurn(before, actor, r.selection)
				}
				if err != nil {
					t.Fatal(err)
				}
				request, err := r.Store.StartRequest(before, turn, actor, "coding", r.selection)
				if err != nil {
					t.Fatal(err)
				}
				warning := llm.Message{Role: "developer", Runtime: true, RequestID: request, Content: prompts.Recovery}
				messages := []llm.Message{
					{Role: "user", Content: "Continue research", InputSource: "task", InputTimeMS: time.Now().UnixMilli()},
					{Role: "assistant", Content: strings.Repeat("Oversized partial evidence. ", 400)},
					warning,
				}
				if actor == "main" {
					if _, err := r.Store.Append(before, turn, actor, "message", "assistant", true, messages[1]); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := r.ensureRecovery(turn, actor, warning); err != nil {
					t.Fatal(err)
				}
				if actor == "main" {
					messages, err = r.Store.Messages(before)
					if err != nil {
						t.Fatal(err)
					}
				}
				retention, err := contextbuild.Retain(messages, 0, 700)
				if err != nil || retention.Start != len(messages) {
					t.Fatal("fixture did not summarize the pending warning", retention, err)
				}
				retained, err := contextbuild.RetainedInputMessages(messages, retention.Inputs, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				// Hashes differ in the actual archive, but path lengths (and token
				// estimates) are fixed. Compute the exact replacement baseline.
				archive, err := r.Store.ArchiveTranscript(before, 0)
				if err != nil {
					t.Fatal(err)
				}
				summary := "Partial evidence summarized; continue carefully."
				baseline := []llm.Message{{Role: "assistant", Content: compactionLinks(summary, archive, archive+".jsonl")}}
				baseline = append(baseline, retained...)
				system := childSystemTemplate
				if actor == "main" || aside {
					system = systemTemplate
				}
				if aside {
					baseline = append(baseline, llm.Message{Role: "developer", Runtime: true, Content: btwInstruction})
				}
				runtime, _, err := r.runtimeContext(context.Background(), actor, r.selection, contextCursor{})
				if err != nil {
					t.Fatal(err)
				}
				input := append(append([]llm.Message(nil), baseline...), *runtime)
				u := estimateUsage(r.selection, system, r.Tools.Definitions(), input)
				warningTokens := contextbuild.Tokens([]llm.Message{warning})
				b := &r.selection.Model.Budget
				b.NextTurnInputReserve = 0
				b.ContextLimit = u.Input + u.Reserved + warningTokens/2
				if fits {
					b.ContextLimit += warningTokens
				}
				if err := b.Validate(); err != nil {
					t.Fatal("fixture budget is invalid", err)
				}
				if !contextbuild.Fits(r.selection, system, r.Tools.Definitions(), input, true) {
					t.Fatal("baseline without warning does not fit")
				}
				withWarning, _ := contextbuild.AppendPendingMessage(baseline, &warning)
				withWarning = append(withWarning, *runtime)
				if contextbuild.Fits(r.selection, system, r.Tools.Definitions(), withWarning, true) != fits {
					t.Fatal("fixture does not straddle the mandatory warning budget")
				}
				summaries := 0
				r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
					summaries++
					if !req.NoTools || !strings.Contains(req.Messages[0].Content, prompts.Recovery) {
						t.Fatal("summary must consume the original warning")
					}
					return emit(llm.StreamEvent{Kind: "text", Text: summary})
				}}
				var result []llm.Message
				if actor == "main" {
					_, err = r.compactContext(context.Background(), "", r.selection, &warning)
					if err == nil {
						result, err = r.Store.Messages(r.Current())
					}
				} else {
					result, _, err = r.compactChild(context.Background(), childTask{actor: actor, turn: turn, selection: r.selection, tools: r.Tools, aside: aside}, messages, contextCursor{}, &warning)
				}
				if summaries != 1 {
					t.Fatal("expected one summary", summaries, err)
				}
				if !fits {
					if err == nil || !strings.Contains(err.Error(), prompts.SessionCompactionHeadroom) || r.Current() != before || result != nil || r.HasNotifications() {
						t.Fatal("handoff committed before fitting the required warning", err, r.Current(), result)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				found := 0
				for _, message := range result {
					if message.Content == warning.Content {
						found++
						if !reflect.DeepEqual(message, warning) {
							t.Fatal("warning provenance changed", message)
						}
					}
				}
				if found != 1 {
					t.Fatal("result omitted or duplicated pending warning", found)
				}
				entries, err := r.Store.Branch(r.Current(), 0)
				if err != nil {
					t.Fatal(err)
				}
				found = 0
				for _, entry := range entries {
					if entry.Actor != actor || entry.Kind != "message" || entry.Role != "developer" {
						continue
					}
					var message llm.Message
					if err := json.Unmarshal(entry.Content, &message); err != nil {
						t.Fatal(err)
					}
					if reflect.DeepEqual(message, warning) {
						found++
					}
				}
				if found != 1 {
					t.Fatal("active branch omitted or duplicated durable warning", found)
				}
			})
		}
	}
}

func TestChildRecoveryCompactionRestoresDurabilityAfterMainHandoff(t *testing.T) {
	for _, aside := range []bool{false, true} {
		t.Run(fmt.Sprint(aside), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			compactionBudget(t, r)
			seedCompactionHistory(t, r, strings.Repeat("Older main evidence. ", 400))
			before := r.Current()
			actor := "main/child_archived_warning"
			turn, err := r.Store.BeginChildTurn(before, actor, r.selection)
			if err != nil {
				t.Fatal(err)
			}
			request, err := r.Store.StartRequest(before, turn, actor, "coding", r.selection)
			if err != nil {
				t.Fatal(err)
			}
			warning := llm.Message{Role: "developer", Runtime: true, RequestID: request, Content: prompts.Recovery}
			if _, err := r.ensureRecovery(turn, actor, warning); err != nil {
				t.Fatal(err)
			}
			messages := []llm.Message{
				{Role: "user", Content: "Continue isolated task", InputSource: "task", InputTimeMS: time.Now().UnixMilli()},
				{Role: "assistant", Content: strings.Repeat("Partial isolated evidence. ", 400)},
				warning,
			}
			summaries := 0
			r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				summaries++
				if strings.Contains(req.ConversationID, actor) {
					// Main archives the child warning while its isolated summary
					// is in flight; the durable entry is not in the new branch.
					if _, err := r.compactContext(ctx, "", r.selection, nil); err != nil {
						return err
					}
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Evidence summarized; continue the task."})
			}}
			result, _, err := r.compactChild(context.Background(), childTask{actor: actor, turn: turn, selection: r.selection, tools: r.Tools, aside: aside}, messages, contextCursor{}, &warning)
			if err != nil || summaries != 2 || r.Current() == before {
				t.Fatal("nested main/child handoff failed", err, summaries)
			}
			if _, added := contextbuild.AppendPendingMessage(result, &warning); added {
				t.Fatal("isolated replacement lost the warning")
			}
			if added, err := r.ensureRecovery(turn, actor, warning); err != nil || added {
				t.Fatal("durable warning was not restored exactly once", added, err)
			}
			main, err := r.Store.Messages(r.Current())
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range main {
				if message.Content == warning.Content {
					t.Fatal("child warning leaked into main context")
				}
			}
		})
	}
}

func TestPartialRecoveryCompactionZeroReserveContinuesAllActors(t *testing.T) {
	for _, actor := range []string{"main", "main/child_zero_reserve", "main/btw_zero_reserve"} {
		t.Run(actor, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			compactionBudget(t, r)
			r.selection.Model.Budget.NextTurnInputReserve = 0
			if err := r.selection.Model.Budget.Validate(); err != nil {
				t.Fatal(err)
			}
			coding, summaries := 0, 0
			var failedRequest int64
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				if req.NoTools {
					summaries++
					return emit(llm.StreamEvent{Kind: "text", Text: "Partial evidence summarized; continue research."})
				}
				coding++
				if coding == 1 {
					if err := r.Store.DB.QueryRow("SELECT max(id) FROM model_requests WHERE actor_id=? AND purpose='coding'", actor).Scan(&failedRequest); err != nil {
						return err
					}
					if err := partialRetryText(emit, strings.Repeat("Oversized partial research output. ", 400)); err != nil {
						return err
					}
					return partialRetryFailure(req)
				}
				warning := partialRetryWarning(t, req)
				if coding != 2 || summaries != 1 || req.PriorAttempts != 1 || warning.RequestID != failedRequest {
					t.Fatal("recovery lost pending warning or attempt count", coding, summaries, req.PriorAttempts, warning)
				}
				if !contextbuild.Fits(req.Selection, req.System, req.Tools, req.Messages, false) {
					t.Fatal("recovery bypassed request admission")
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Recovered after compaction"})
			}}
			var err error
			if actor == "main" {
				err = r.Run(&llm.Message{Role: "user", Content: "Recover this task"})
			} else {
				seedRuntime(t, r, "Private main context")
				turn, e := r.Store.BeginChildTurn(r.Current(), actor, r.selection)
				if e != nil {
					t.Fatal(e)
				}
				err = r.runChild(context.Background(), childTask{actor: actor, turn: turn, prompt: "Isolated task", selection: r.selection, tools: r.Tools, aside: strings.Contains(actor, "/btw_")}, io.Discard, io.Discard)
				partialRetryNoMainLeak(t, r)
			}
			if err != nil || coding != 2 || summaries != 1 {
				t.Fatal("expected one compaction before recovery", err, coding, summaries)
			}
		})
	}
}
