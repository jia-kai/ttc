package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func TestWireProjectsSemanticTypesWithoutReclassifyingUserJSON(t *testing.T) {
	content := `{"type":"job_exit","job_id":"job_1"}`
	types := []llm.MessageType{llm.TypeUserInput, llm.TypeRuntimeNotification, llm.TypeRuntimeContext, llm.TypeCompactionContext, llm.TypeRuntimeInstruction, llm.TypeSystemInstruction, llm.TypeDeveloperInstruction}
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
		if typ == llm.TypeRuntimeNotification {
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

func TestWireSteeringIsExplicitAndKeepsAttachments(t *testing.T) {
	const text = "Only investigate.\n\"quoted\" \\ path <tag>"
	file := llm.BinaryFile{Path: "/snapshot/evidence.png", MIMEType: "image/png", Bytes: 4}
	messages := []llm.Message{
		{Type: llm.TypeUserInput, InputSource: "steer", Content: text, Files: []llm.BinaryFile{file}},
		{Type: llm.TypeChildGuidance, Content: `{"type":"child_message","message":"Check the evidence","job_id":"routing_only","event_seq":25,"job_read_grants":["readable"]}`},
		{Type: llm.TypeUserInput, InputSource: "steer", Files: []llm.BinaryFile{file}},
	}
	before, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	selection := llm.Selection{Provider: "openai", Model: llm.ScriptModel()}
	selection.Model.Images = true
	resolved := 0
	body, err := wire(context.Background(), llm.Request{ConversationID: "steering", Selection: selection, Messages: messages}, func(_ context.Context, got llm.BinaryFile) (llm.BinaryPayload, error) {
		resolved++
		if !reflect.DeepEqual(got, file) {
			t.Fatal(got)
		}
		return llm.BinaryPayload{Data: []byte("data"), MIMEType: "image/png"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Input []struct {
			Role    string
			Content []map[string]any
		}
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != 3 || resolved != 2 {
		t.Fatal(string(body), resolved)
	}
	wantTexts := []string{text, "Check the evidence", ""}
	for i, item := range req.Input {
		if item.Role != "user" || item.Content[0]["type"] != "input_text" {
			t.Fatal(item)
		}
		var steer map[string]any
		if err := json.Unmarshal([]byte(item.Content[0]["text"].(string)), &steer); err != nil {
			t.Fatal(err)
		}
		source, fields := "user", 3
		if i == 1 {
			source, fields = "main", 4
		}
		if steer["type"] != "steer" || steer["source"] != source || steer["message"] != wantTexts[i] || len(steer) != fields {
			t.Fatal("wrong receiver-facing steering", steer)
		}
		if i == 1 {
			if len(item.Content) != 1 || !reflect.DeepEqual(steer["job_read_grants"], []any{"readable"}) {
				t.Fatal(item)
			}
		} else if len(item.Content) != 2 || item.Content[1]["type"] != "input_image" || item.Content[1]["image_url"] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("data")) {
			t.Fatal("steering attachment changed", item)
		}
	}
	after, err := json.Marshal(messages)
	if err != nil || string(before) != string(after) {
		t.Fatal("canonical messages changed", err)
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
