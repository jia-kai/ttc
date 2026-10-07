package history

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"ttc/internal/llm"
	"ttc/internal/render"
)

func TestInspectionPagesBoundLargeToolAndMessageBodies(t *testing.T) {
	s, v, turn, req := historyFixture(t)
	call := llm.ToolCall{ID: "large", Name: "read", Arguments: json.RawMessage(`{"path":"fixture.txt"}`)}
	_, calls, err := s.Assistant(v.ID, turn, "main", req, llm.Message{Role: "assistant", Calls: []llm.ToolCall{call}})
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("界", (8<<20)/3) + "Last line"
	md := render.Markdown{Revision: 1, Summary: "read · fixture.txt", Detail: body}
	id, err := s.CallResult(v.ID, turn, "main", calls[0], json.RawMessage(`{"ok":true}`), nil, nil, md, true)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.InspectPage(context.Background(), id, 0, InspectionPageChars)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Supported || !page.Markdown || page.Total != utf8.RuneCountInString(body) || utf8.RuneCountInString(page.Text) != InspectionPageChars || len(page.Text) > 64<<10 {
		t.Fatal("unbounded or invalid first page", page.Total, len(page.Text), err)
	}
	end := ((page.Total - 1) / page.Limit) * page.Limit
	tail, err := s.InspectPage(context.Background(), id, end, page.Limit)
	if err != nil || !strings.HasSuffix(tail.Text, "Last line") || len(tail.Text) > 64<<10 {
		t.Fatal("incorrect last page", err)
	}
	messageID, err := s.Append(v.ID, turn, "main", "message", "assistant", true, llm.Message{Role: "assistant", Content: strings.Repeat("x", 1<<20)})
	if err != nil {
		t.Fatal(err)
	}
	message, err := s.InspectPage(context.Background(), messageID, InspectionPageChars, InspectionPageChars)
	if err != nil || message.Total != 1<<20 || len(message.Text) != InspectionPageChars {
		t.Fatal("message paging failed", message.Total, len(message.Text), err)
	}
}

func TestInstructionInspectionPagesAndValidation(t *testing.T) {
	s, v, turn, req := historyFixture(t)
	text := strings.Repeat("λ", 10000) + "end"
	id, err := s.RecordSystemPrompt(v.ID, turn, "main", req, text)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.InspectPage(context.Background(), id, 10000, 100)
	if err != nil || !page.System || page.Markdown || page.Total != 10003 || page.Text != "end" {
		t.Fatal("instruction character paging", page, err)
	}
	for _, input := range [][2]int{{-1, 10}, {0, 0}, {0, InspectionPageChars + 1}} {
		if _, err := s.InspectPage(context.Background(), id, input[0], input[1]); err == nil {
			t.Fatal("invalid page accepted", input)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.InspectPage(ctx, id, 0, 100); err == nil {
		t.Fatal("canceled page read succeeded")
	}
}

func TestInspectionTinyMessageOmitsBinaryReferencesAndLargeReplayEnvelopes(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	for _, message := range []llm.Message{
		{Role: "user", Files: []llm.BinaryFile{{Path: "/snapshot/fixture.png", SHA256: strings.Repeat("a", 64), MIMEType: "image/png", Bytes: 8 << 20}}},
		{Role: "assistant", Content: "Done.", State: &llm.ReplayState{Provider: "fixture", Model: "fixture", Version: 1, Items: []json.RawMessage{json.RawMessage(`{"opaque":"` + strings.Repeat("x", 8<<20) + `"}`)}}},
	} {
		id, err := s.Append(v.ID, turn, "main", "message", message.Role, true, message)
		if err != nil {
			t.Fatal(err)
		}
		page, err := s.InspectPage(context.Background(), id, 0, InspectionPageChars)
		if err != nil || !page.Supported || page.LargeEnvelope != (message.Role == "assistant") || len(page.Text) > 100 || page.Total > page.Limit {
			t.Fatal("inspection lost bounded text or native envelope size", page, err)
		}
		if message.Role == "user" && !strings.Contains(page.Text, "Binary snapshot: /snapshot/fixture.png") {
			t.Fatal("image-only inspection lost snapshot metadata", page.Text)
		}
	}
}
