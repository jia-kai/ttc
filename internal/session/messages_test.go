package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"ttc/internal/llm"
	"ttc/internal/prompts"
	"ttc/internal/tool"
)

func TestSessionToolMessageAssets(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	for _, test := range []struct {
		name, tool, actor, args, code, message string
	}{
		{"persistence", "subagent", "main", `{"prompt":"audit","label":"audit"}`, "invalid_input", "invalid_arguments: " + prompts.ChildExplicitPersistence},
		{"variant", "subagent", "main", `{"prompt":"audit","label":"audit","persistent":false,"variant":""}`, "invalid_input", "invalid_arguments: " + prompts.ChildNonemptyVariant},
		{"follow-up label", "subagent", "main", `{"prompt":"audit","child_id":"child","label":"audit","persistent":false}`, "invalid_input", "invalid_arguments: " + prompts.ChildFollowupLabel},
		{"label", "subagent", "main", `{"prompt":"audit","label":"one two three four five","persistent":false}`, "invalid_input", "invalid_arguments: " + prompts.ChildValidLabel},
		{"child assignment", "subagent", "main/child", `{"prompt":"audit","label":"audit","persistent":false}`, "ownership", prompts.ChildCannotAssign},
		{"question count", "question", "main", `{"questions":[]}`, "invalid_input", prompts.QuestionCount},
		{"question identity", "question", "main", `{"questions":[{"id":"q","prompt":""}]}`, "invalid_input", prompts.QuestionIdentityAndPrompt},
		{"choice count", "question", "main", `{"questions":[{"id":"q","prompt":"Q?","options":[]}]}`, "invalid_input", prompts.QuestionChoiceCount},
		{"option identity", "question", "main", `{"questions":[{"id":"q","prompt":"Q?","options":[{"id":"a","label":"A"},{"id":"a","label":"B"}]}]}`, "invalid_input", prompts.QuestionOptionIdentityAndLabel},
		{"recommendation", "question", "main", `{"questions":[{"id":"q","prompt":"Q?","recommended_option_id":"a"}]}`, "invalid_input", prompts.QuestionRecommendation},
		{"child question", "question", "main/child", `{"questions":[{"id":"q","prompt":"Q?"}]}`, "execution_failed", prompts.QuestionMainOnly},
		{"wakeup time", "wakeup_schedule", "main", `{"name":"audit","message":"check"}`, "invalid_input", prompts.WakeupTimeRequired},
		{"wakeup delay", "wakeup_schedule", "main", `{"name":"audit","message":"check","delay_seconds":-1}`, "invalid_input", prompts.WakeupDelayRange},
		{"wakeup repeat", "wakeup_schedule", "main", `{"name":"audit","message":"check","delay_seconds":1,"repeat_seconds":0}`, "invalid_input", prompts.WakeupRepeatRange},
		{"wakeup cancel identity", "wakeup_cancel", "main", `{}`, "invalid_input", prompts.WakeupCancelIdentity},
		{"wakeup not found", "wakeup_cancel", "main", `{"name":"missing"}`, "not_found", prompts.WakeupCancelNotFound},
		{"unsupported clicks", "image_show", "main", `{"path":"missing.png","request_click":true}`, "unsupported_interaction", prompts.ImageClicksUnsupported},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := r.Tools.Invoke(context.Background(), tool.Execution{Actor: test.actor}, test.tool, json.RawMessage(test.args))
			var result struct {
				OK    bool       `json:"ok"`
				Error tool.Error `json:"error"`
			}
			if err := json.Unmarshal(record.Result, &result); err != nil {
				t.Fatal(err)
			}
			if result.OK || result.Error.Code != test.code || result.Error.Message != test.message {
				t.Fatalf("got %s; want %s: %q", record.Result, test.code, test.message)
			}
		})
	}
}

func TestSessionNamingScaffoldBytes(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Name this session")
	user := strings.Repeat("x", 4095) + "πtail"
	assistant := llm.Message{Role: "assistant", Content: "raw %s\nassistant", Calls: []llm.ToolCall{{Name: "read"}, {Name: "glob"}, {Name: "grep"}, {Name: "shell"}, {Name: "ignored"}}}
	calls := 0
	r.Provider = &childProvider{stream: func(_ context.Context, request llm.Request, emit func(llm.StreamEvent) error) error {
		calls++
		want := strings.Repeat("x", 4095) + "\n[truncated]\n\nraw %s\nassistant\nTool: read\nTool: glob\nTool: grep\nTool: shell"
		if len(request.Messages) != 1 || request.Messages[0].Content != want {
			t.Fatalf("naming input bytes changed: %#v", request.Messages)
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Small Research Tool Session"})
	}}
	r.name(context.Background(), r.Current(), r.CurrentSelection(), "", llm.Message{Role: "user", Content: user}, assistant)
	if calls != 1 {
		t.Fatalf("naming calls = %d, want 1", calls)
	}
}

func TestSessionSummaryScaffoldPreservesRawBytes(t *testing.T) {
	messages := []llm.Message{
		{Role: "assistant", Content: "raw %s\nπ", Calls: []llm.ToolCall{{ID: "c%1", Name: "read", Arguments: json.RawMessage("{\"path\": \"a%b\"}")}}},
		{Role: "tool", CallID: "c%1", Content: "  {\"ok\":true}\n", Files: []llm.BinaryFile{{MIMEType: "image/png", Path: "/raw %s.png"}}},
	}
	want := "\n### assistant\nraw %s\nπ\nTool call c%1 · read\n{\"path\": \"a%b\"}\n\n### tool · c%1\n  {\"ok\":true}\n\nBinary attachment (image/png): /raw %s.png\n"
	if got := summaryTranscript(messages); got != want {
		t.Fatalf("summary transcript bytes changed:\ngot  %q\nwant %q", got, want)
	}
}

func TestSessionUnusableContextMessagePreservesDiagnostic(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.fatalCompaction = "raw %w\nπ failure"
	want := "session unusable after compaction: raw %w\nπ failure; inspect/export history or start/load another session"
	if err := r.checkContext(); err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	result := failedTool(llm.ToolCall{Name: "read"}, "interrupted", prompts.SessionToolInterrupted)
	var value struct {
		Error tool.Error `json:"error"`
	}
	if err := json.Unmarshal(result.Result, &value); err != nil {
		t.Fatal(err)
	}
	if value.Error.Message != "stream did not complete; tool was not executed" {
		t.Fatal(value.Error)
	}
}

func TestSessionCompactionDiagnosticAssets(t *testing.T) {
	for _, test := range []struct {
		name, message string
		event         llm.StreamEvent
	}{
		{"tool call", prompts.SessionCompactionNoTools, llm.StreamEvent{Kind: "call"}},
		{"oversized", prompts.SessionCompactionSummaryTooLarge, llm.StreamEvent{Kind: "text", Text: strings.Repeat("x", (1<<20)+1)}},
		{"empty", prompts.SessionCompactionEmptySummary, llm.StreamEvent{Kind: "text", Text: " \n"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			seedRuntime(t, r, "Compact this session")
			r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
				return emit(test.event)
			}}
			_, err := r.summarize(context.Background(), "main/child", "", r.CurrentSelection(), "raw input", "")
			if err == nil || err.Error() != test.message {
				t.Fatalf("got %v, want %q", err, test.message)
			}
		})
	}
	r, _ := runtimeFixture(t, nil)
	_, _, err := r.compactChild(context.Background(), childTask{}, nil, contextCursor{}, nil)
	if err == nil || err.Error() != prompts.ChildCompactionIdentity {
		t.Fatalf("got %v, want %q", err, prompts.ChildCompactionIdentity)
	}
	if got := fmt.Sprintf(prompts.SessionCompactionInputTooLarge, 1, 2, 3); got != "compaction input exceeds model context (estimated input 1 + output/margin reserve 2, limit 3 tokens); no summary was requested" {
		t.Fatal(got)
	}
}
