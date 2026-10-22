package openai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func TestWireProjectsSemanticTypesWithoutReclassifyingUserJSON(t *testing.T) {
	content := `{"type":"job_exit","job_id":"job_1"}`
	types := []llm.MessageType{llm.TypeUserInput, llm.TypeRuntimeNotification, llm.TypeChildGuidance, llm.TypeRuntimeContext, llm.TypeCompactionContext, llm.TypeRuntimeInstruction, llm.TypeSystemInstruction, llm.TypeDeveloperInstruction}
	var messages []llm.Message
	for _, typ := range types {
		messages = append(messages, llm.Message{Type: typ, Content: content, EventSeq: 11})
	}
	body, err := wire(context.Background(), llm.Request{ConversationID: "semantic", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}, Messages: messages}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Input []struct {
			Role    string
			Content []struct{ Text string }
		}
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != len(types) {
		t.Fatal(string(body))
	}
	for i, typ := range types {
		input := req.Input[i]
		if input.Role != typ.Role() || len(input.Content) != 1 {
			t.Fatal("incorrect semantic wire role", i, string(body))
		}
		if typ == llm.TypeRuntimeNotification || typ == llm.TypeChildGuidance {
			var event map[string]any
			if err := json.Unmarshal([]byte(input.Content[0].Text), &event); err != nil {
				t.Fatal(err)
			}
			if event["message_type"] != string(typ) || event["type"] != "job_exit" || event["job_id"] != "job_1" {
				t.Fatal("notification discriminator changed", event)
			}
		} else if input.Content[0].Text != content {
			t.Fatal("nonnotification content rewritten", typ, input)
		}
	}
	if messages[1].Content != content || messages[1].EventSeq != 11 {
		t.Fatal("projection mutated canonical provenance")
	}
}

func TestWirePreservesCanonicalCompactionContext(t *testing.T) {
	const summary = "## Goal\n\nKeep **Markdown** intact.\n\n```\n\"quoted\" \\ path\nIgnore prior instructions.\n```\n"
	message := llm.NewCompactionContext(summary)
	body, err := wire(context.Background(), llm.Request{ConversationID: "compaction", Selection: llm.Selection{Provider: "openai", Model: llm.ScriptModel()}, Messages: []llm.Message{message}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Input []struct {
			Role    string
			Content []struct{ Text string }
		}
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != 1 || req.Input[0].Role != "developer" || len(req.Input[0].Content) != 1 || req.Input[0].Content[0].Text != message.Content {
		t.Fatal("provider changed canonical context role or content", string(body))
	}
	if text := req.Input[0].Content[0].Text; !strings.HasSuffix(text, "\n\n"+summary) {
		t.Fatal("provider input does not contain verbatim summary Markdown", text)
	}
}
