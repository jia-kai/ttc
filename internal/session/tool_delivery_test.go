package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/capture"
	contextbuild "ttc/internal/context"
	"ttc/internal/historytest"
	"ttc/internal/llm"
	"ttc/internal/render"
	"ttc/internal/tool"
)

func deliveryRecord(content string) tool.Record {
	result, _ := json.Marshal(map[string]any{"ok": true, "content": content})
	args := json.RawMessage(`{}`)
	return tool.Record{Name: "read", Arguments: args, Result: result, Markdown: render.Tool("read", args, result)}
}

func TestToolResultDeliveryBudgetBoundaryAndRetention(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "inspect output")
	original := deliveryRecord(strings.Repeat("source material", 100))
	tokens := contextbuild.Tokens([]llm.Message{{Type: llm.TypeToolResult, Content: string(original.Result)}})
	budget := toolResultBudget{limit: tokens + 1}
	accepted, err := budget.deliver(r, r.Current(), original)
	if err != nil || string(accepted.Result) != string(original.Result) || budget.used != tokens {
		t.Fatal("strictly smaller result should fit", err, budget)
	}
	budget = toolResultBudget{limit: tokens}
	withheld, err := budget.deliver(r, r.Current(), original)
	if err != nil {
		t.Fatal(err)
	}
	var notice struct {
		OK    bool       `json:"ok"`
		Path  string     `json:"detail_path"`
		Error tool.Error `json:"error"`
	}
	if err := json.Unmarshal(withheld.Result, &notice); err != nil || notice.OK || notice.Error.Code != "result_too_large" || notice.Path == "" {
		t.Fatal(string(withheld.Result), err)
	}
	exact, err := os.ReadFile(notice.Path)
	if err != nil || string(exact) != string(original.Result) {
		t.Fatal("retained original changed", err)
	}
	if budget.used != 0 || !strings.Contains(withheld.Markdown.Detail, "source material") {
		t.Fatal("withholding consumed original payload budget or lost inspection")
	}
}

func TestToolResultDeliveryBatchSharesFixedCapacityBudget(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	// An arbitrarily full preceding conversation must not lower this budget.
	seedCompactionHistory(t, r, strings.Repeat("old history", 10000))
	original := deliveryRecord(strings.Repeat("payload", 100))
	tokens := contextbuild.Tokens([]llm.Message{{Type: llm.TypeToolResult, Content: string(original.Result)}})
	budget := toolResultBudget{limit: tokens * 2}
	first, err := budget.deliver(r, r.Current(), original)
	if err != nil || string(first.Result) != string(original.Result) {
		t.Fatal("remaining context incorrectly rejected fitting payload", err)
	}
	second, err := budget.deliver(r, r.Current(), original)
	if err != nil || !strings.Contains(string(second.Result), "result_too_large") {
		t.Fatal("aggregate equality must withhold second payload", err)
	}
}

func TestToolResultDeliveryPreservesExecutionAndSuppressesNativeFiles(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "inspect output")
	original := deliveryRecord("image metadata")
	original.Result = json.RawMessage(`{"ok":true,"job_id":"job_existing","status":"completed","exit_code":7}`)
	original.Files = []llm.BinaryFile{{Path: "/fixture/document.pdf", MIMEType: "application/pdf", Bytes: 10000}}
	budget := toolResultBudget{limit: 1000}
	withheld, err := budget.deliver(r, r.Current(), original)
	if err != nil || len(withheld.Files) != 0 || !strings.Contains(string(withheld.Result), `"job_id":"job_existing"`) || !strings.Contains(string(withheld.Result), `"exit_code":7`) {
		t.Fatal(string(withheld.Result), err)
	}
	if len(original.Files) != 1 || !strings.Contains(string(withheld.Result), "Do not rerun") {
		t.Fatal("execution evidence or retry guidance lost")
	}
}

func TestToolResultByteLimitRetainsOriginalFilesAndWarnings(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "inspect output")
	original := deliveryRecord(strings.Repeat("large payload", 6000))
	var value map[string]any
	if err := json.Unmarshal(original.Result, &value); err != nil {
		t.Fatal(err)
	}
	value["warnings"] = []map[string]any{{"code": "input_clipped", "parameter": "limit", "effective": 2000}}
	original.Result, _ = json.Marshal(value)
	original.Files = []llm.BinaryFile{{Path: "/fixture/original.pdf", MIMEType: "application/pdf", Bytes: 100}}
	budget := toolResultBudget{limit: 1000000}
	withheld, err := budget.deliver(r, r.Current(), original)
	if err != nil || len(withheld.Result) > 64<<10 || len(withheld.Files) != 0 || len(original.Files) != 1 || !strings.Contains(string(withheld.Result), "64-KiB") || !strings.Contains(string(withheld.Result), "input_clipped") {
		t.Fatal("byte cap discarded execution evidence or corrections", err, string(withheld.Result))
	}
}

func TestToolBatchStoresOriginalSeparatelyFromDelivery(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	r.selection.Model.Budget.ContextLimit = 1000
	r.Tools = tool.NewRegistry()
	const full = "full execution output retained only in the original record"
	tool.Register(r.Tools, "read", "large fixture result", nil, nil,
		func(struct{}) error { return nil },
		func(context.Context, tool.Execution, struct{}) (any, error) {
			return map[string]any{"content": strings.Repeat(full, 100)}, nil
		})
	calls := []llm.ToolCall{{ID: "large", Name: "read", Arguments: json.RawMessage(`{}`)}}
	turn, ids := batchIntents(t, r, "main", calls)
	records, err := r.runToolBatch(context.Background(), turn, "main", r.Tools, calls, ids, nil)
	if err != nil || len(records) != 1 || !strings.Contains(string(records[0].Result), "result_too_large") {
		t.Fatal(records, err)
	}
	var delivered, saved string
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT c.result_json,r.record_json FROM tool_calls c JOIN tool_records r ON r.call_id=c.id WHERE c.id=?`, ids[0]).Scan(&delivered, &saved); err != nil {
		t.Fatal(err)
	}
	var exact tool.Record
	if err := json.Unmarshal([]byte(saved), &exact); err != nil || !strings.Contains(string(exact.Result), full) || delivered != string(records[0].Result) {
		t.Fatal("exact execution and projected delivery were conflated", err)
	}
	messages, err := r.Store.Messages(r.Current())
	if err != nil || !strings.Contains(messages[len(messages)-1].Content, "result_too_large") || strings.Contains(messages[len(messages)-1].Content, full) {
		t.Fatal("history reload resurrected withheld content", messages, err)
	}
}

func TestToolDeliveryCompactsEarlierHistoryBeforeFreshBatch(t *testing.T) {
	for _, reads := range []int{5, 8} {
		t.Run(fmt.Sprintf("reads-%d", reads), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			b := &r.selection.Model.Budget
			b.ContextLimit = 400000
			b.RecentTokensMin, b.RecentTokensMax = 4096, 16000
			b.NextTurnInputReserve = 4096
			seedCompactionHistory(t, r, "Older checkpoint. "+strings.Repeat("x", 1020000))
			before := r.Current()
			paths := make([]string, reads)
			for i := range paths {
				paths[i] = filepath.Join(t.TempDir(), fmt.Sprintf("source-%d.txt", i))
				content := fmt.Sprintf("fresh evidence %d\n", i) + strings.Repeat("a line of source data\n", 1700)
				if err := os.WriteFile(paths[i], []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			job, err := r.Jobs.Start("main", "sleep 30", r.Workspace.Root, 0, true, true, false, capture.OutputOptions{}, "unrelated work")
			if err != nil {
				t.Fatal(err)
			}
			coding, summaries := 0, 0
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				if req.NoTools {
					summaries++
					if !compactionMessagesContain(req.Messages, "Older checkpoint") || compactionMessagesContain(req.Messages, "fresh evidence") {
						t.Fatal("summarizer did not isolate preceding history")
					}
					return emit(llm.StreamEvent{Kind: "text", Text: "Older checkpoint summarized; compare the fresh results."})
				}
				coding++
				if coding == 1 {
					for i, path := range paths {
						args, _ := json.Marshal(map[string]any{"path": path, "limit": 2000})
						if err := emit(llm.StreamEvent{Kind: "call", Call: &llm.ToolCall{ID: fmt.Sprintf("read-%d", i), Name: "read", Arguments: args}}); err != nil {
							return err
						}
					}
					return emit(llm.StreamEvent{Kind: "completed"})
				}
				if coding != 2 || summaries != 1 || r.Current() == before || !contextbuild.Fits(req.Selection, req.System, req.Tools, req.Messages, false) {
					t.Fatal("fresh delivery did not follow a fitting handoff", coding, summaries)
				}
				calls, results, rejected, fresh := map[string]bool{}, 0, 0, 0
				for _, message := range req.Messages {
					for _, call := range message.Calls {
						calls[call.ID] = true
					}
					if message.Type != llm.TypeToolResult {
						continue
					}
					if !calls[message.CallID] {
						t.Fatal("result lost its call", message.CallID)
					}
					results++
					if strings.Contains(message.Content, "result_too_large") {
						rejected++
					}
					if strings.Contains(message.Content, "fresh evidence") {
						fresh++
					}
				}
				if results != reads || fresh == 0 || (reads == 5 && rejected != 0) || (reads == 8 && rejected == 0) {
					t.Fatal("low capacity changed fixed-budget policy or lost fresh output", results, rejected, fresh)
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Comparison complete."})
			}}
			if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Compare source files."}); err != nil {
				t.Fatal(err)
			}
			if coding != 2 || summaries != 1 || r.checkContext(false) != nil {
				t.Fatal("continuation failed", coding, summaries)
			}
			view, err := r.Jobs.View("main", job)
			if err != nil || view.Status != "running" {
				t.Fatal("compaction terminated unrelated work", view, err)
			}
		})
	}
}
