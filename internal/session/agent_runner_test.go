package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"ttc/internal/historytest"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// Exercise all entry paths against the same failed/successful request sequence:
// the failed request's calls must settle before recovery and its usage must count.
func TestAgentRunnerSharedRecoveryAccountingAndPresentation(t *testing.T) {
	for _, role := range []string{"main", "child", "aside"} {
		t.Run(role, func(t *testing.T) {
			r, events := runtimeFixture(t, nil)
			step := 0
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				step++
				usage := &llm.Usage{InputTokens: 10 * step, OutputTokens: step}
				switch step {
				case 1, 2:
					if req.PriorAttempts != 0 {
						t.Errorf("successful cycle did not reset retry accounting: %d", req.PriorAttempts)
					}
					if step == 2 {
						partialRetryResult(t, req, "settled", true, "")
						if err := partialRetryText(emit, "Partial evidence"); err != nil {
							return err
						}
					}
					id := "settled"
					if step == 2 {
						id = "interrupted"
					}
					call := llm.ToolCall{ID: id, Name: "read", Arguments: []byte(`{"path":"."}`)}
					if err := emit(llm.StreamEvent{Kind: "call", Call: &call}); err != nil {
						return err
					}
					if err := emit(llm.StreamEvent{Kind: "completed", Usage: usage}); err != nil {
						return err
					}
					if step == 2 {
						return partialRetryFailure(req)
					}
					return nil
				case 3:
					if req.PriorAttempts != 1 {
						t.Errorf("partial continuation prior attempts = %d", req.PriorAttempts)
					}
					partialRetryResult(t, req, "settled", true, "")
					partialRetryResult(t, req, "interrupted", false, "interrupted")
					partialRetryWarning(t, req)
					if message := partialRetryMessage(t, req, "assistant", "Partial evidence"); message.State != nil {
						t.Error("failed native state was replayed")
					}
					if err := emit(llm.StreamEvent{Kind: "text", Text: "Final answer"}); err != nil {
						return err
					}
					return emit(llm.StreamEvent{Kind: "completed", Usage: usage})
				default:
					return errors.New("unexpected extra model request")
				}
			}}
			var stdout bytes.Buffer
			if role == "main" {
				completion, err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Run recovery cycle"})
				if err != nil || completion == nil || strings.Contains(completion.Text, "N/A") {
					t.Fatalf("main completion = %+v, error = %v", completion, err)
				}
			} else {
				seedRuntime(t, r, "Main context")
				actor := "main/child_runner"
				if role == "aside" {
					actor = "main/btw_runner"
				}
				turn, err := r.Store.BeginChildTurn(r.Current(), actor, r.CurrentSelection())
				if err != nil {
					t.Fatal(err)
				}
				err = r.runChild(context.Background(), childTask{
					actor: actor, turn: turn, prompt: "Run recovery cycle", selection: r.CurrentSelection(),
					tools: r.Tools.Filter(readOnlyTool), aside: role == "aside",
				}, &stdout, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				if err := r.Store.FinishTurn(turn, "completed"); err != nil {
					t.Fatal(err)
				}
				want := "Partial evidenceFinal answer"
				if role == "aside" {
					want = "Final answer"
				}
				if stdout.String() != want {
					t.Fatalf("%s output = %q, want %q", role, stdout.String(), want)
				}
			}
			totals := r.UsageSnapshot().Totals
			if step != 3 || totals.Requests != 3 || totals.ReportedRequests != 3 || totals.Tokens.InputTokens != 60 || totals.Tokens.OutputTokens != 6 {
				t.Fatalf("requests = %d, totals = %+v", step, totals)
			}
			var requests, failed, outcomes int
			if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*),sum(status='failed') FROM model_requests WHERE purpose='coding'`).Scan(&requests, &failed); err != nil {
				t.Fatal(err)
			}
			if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM entries WHERE kind='tool_result'`).Scan(&outcomes); err != nil {
				t.Fatal(err)
			}
			if requests != 3 || failed != 1 || outcomes != 2 {
				t.Fatalf("saved requests = %d, failed = %d, tool outcomes = %d", requests, failed, outcomes)
			}
			if role == "aside" {
				for len(events) != 0 {
					event := <-events
					if event.Kind == "delta" || event.Kind == "assistant" {
						t.Errorf("aside exposed private response event: %+v", event)
					}
				}
			}
		})
	}
}

func TestAgentRunnerAssignmentTextLimitsDoNotAuthorizeRecovery(t *testing.T) {
	for _, tc := range []struct {
		actor string
		limit int
		aside bool
	}{
		{actor: "main/child_bounded", limit: 32 << 20},
		{actor: "main/btw_bounded", limit: 64 << 10, aside: true},
	} {
		t.Run(tc.actor, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			seedRuntime(t, r, "Main context")
			requests := 0
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				requests++
				if err := partialRetryText(emit, "Accepted prefix"); err != nil {
					return err
				}
				callbackErr := emit(llm.StreamEvent{Kind: "text", Text: strings.Repeat("x", tc.limit)})
				if callbackErr == nil {
					return errors.New("expected response limit error")
				}
				failure := partialRetryFailure(req)
				failure.Err = callbackErr // A provider wrapper cannot turn a local limit into recovery permission.
				return failure
			}}
			turn, err := r.Store.BeginChildTurn(r.Current(), tc.actor, r.CurrentSelection())
			if err != nil {
				t.Fatal(err)
			}
			var stdout bytes.Buffer
			err = r.runChild(context.Background(), childTask{
				actor: tc.actor, turn: turn, prompt: "Bound this response", selection: r.CurrentSelection(),
				tools: r.Tools.Filter(readOnlyTool), aside: tc.aside,
			}, &stdout, io.Discard)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf(prompts.ChildResponseTooLarge, tc.limit)) {
				t.Fatalf("limit error = %v", err)
			}
			if requests != 1 {
				t.Fatalf("local text-limit failure retried %d requests", requests)
			}
			want := "Accepted prefix"
			if tc.aside {
				want = ""
			}
			if stdout.String() != want {
				t.Fatalf("output = %q, want %q", stdout.String(), want)
			}
			if err := r.Store.FinishTurn(turn, "failed"); err != nil {
				t.Fatal(err)
			}
			entries, err := r.Store.Branch(r.Current(), 0)
			if err != nil {
				t.Fatal(err)
			}
			seenPartial := false
			for _, entry := range entries {
				if entry.Kind != "message" || entry.Actor != tc.actor {
					continue
				}
				view, err := r.Store.ReadView(context.Background(), entry)
				if err != nil {
					t.Fatal(err)
				}
				if view.Message == nil {
					continue
				}
				if view.Message.Content == prompts.Recovery {
					t.Error("local failure added a recovery warning")
				}
				if view.Message.Type == llm.TypeAssistant {
					seenPartial = true
					if view.Message.State != nil || view.Message.Content != "Accepted prefix" {
						t.Errorf("incorrect partial output: %+v", view.Message)
					}
				}
			}
			if !seenPartial {
				t.Error("local text-limit failure lost partial assistant evidence")
			}
		})
	}
}
