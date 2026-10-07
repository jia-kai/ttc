package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gdamore/tcell/v2"
	"os"
	"path/filepath"
	"testing"
	"ttc/internal/llm"
)

type pendingProvider struct {
	llm.Script
	release  chan struct{}
	requests int
}

func (p *pendingProvider) Stream(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
	p.requests++
	if p.requests > 1 {
		if err := emit(llm.StreamEvent{Kind: "text", Text: "Streamed write finished."}); err != nil {
			return err
		}
		return emit(llm.StreamEvent{Kind: "completed"})
	}
	if err := emit(llm.StreamEvent{Kind: "call_start", CallStart: &llm.ToolStart{ID: "write_call", Name: "write"}}); err != nil {
		return err
	}
	if err := emit(llm.StreamEvent{Kind: "call_progress", CallProgress: &llm.ToolProgress{ID: "write_call", Name: "write", Segments: 2, Bytes: 9}}); err != nil {
		return err
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	args := json.RawMessage(`{"path":"stream.txt","content":"assembled\n"}`)
	if err := emit(llm.StreamEvent{Kind: "call", Call: &llm.ToolCall{ID: "write_call", Name: "write", Arguments: args}}); err != nil {
		return err
	}
	return emit(llm.StreamEvent{Kind: "completed"})
}
func TestAwaitingToolInspectorAndTransition(t *testing.T) {
	p := &pendingProvider{release: make(chan struct{})}
	u := newQuestionTestUI(t, p)
	u.typeText("write a streamed file")
	u.key(tcell.KeyEnter)
	u.wait(t, "awaiting write (2seg/9bytes) ...")
	var count int
	if err := u.runtime.Store.DB.QueryRow("SELECT count(*) FROM tool_calls").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("announcement became executable intent")
	}
	if _, err := os.Stat(filepath.Join(u.runtime.Workspace.Root, "stream.txt")); !os.IsNotExist(err) {
		t.Fatal("tool ran before complete arguments", err)
	}
	var id int64
	if err := u.runtime.Store.DB.QueryRow("SELECT id FROM entries WHERE json_extract(content_json,'$.type')='tool_stream'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	u.typeText(fmt.Sprintf("/inspect %d", id))
	u.key(tcell.KeyEnter)
	u.wait(t, "call_id")
	close(p.release)
	u.wait(t, "assembled") // The open announcement inspector refreshes to exact tool details.
	u.key(tcell.KeyEscape)
	u.wait(t, "Streamed write finished.")
	b, err := os.ReadFile(filepath.Join(u.runtime.Workspace.Root, "stream.txt"))
	if err != nil || string(b) != "assembled\n" {
		t.Fatal(string(b), err)
	}
}
