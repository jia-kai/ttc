package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextbuild "scicode/internal/context"
	"scicode/internal/provider"
)

func TestRetainedInputsRepeatedCompactionUndoRedoBaseline(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	seedRuntime(t, r, "First ordinary prompt")
	r.selection.Model.Budget.RecentTokensMin = 0
	r.selection.Model.Budget.RecentTokensMax = 700
	r.selection.Model.Budget.ContextLimit = 1 << 20
	step := 0
	r.Provider = &childProvider{stream: func(_ context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
		if req.NoTools {
			return emit(provider.StreamEvent{Kind: "text", Text: "File baseline established; only the suffix is undoable."})
		}
		step++
		if step == 1 {
			for _, text := range []string{"First committed steer", "Second committed steer"} {
				if err := r.Steer(contextbuild.Input{Text: text}); err != nil {
					return err
				}
			}
		}
		if step == 4 {
			return emit(provider.StreamEvent{Kind: "text", Text: "Suffix complete."})
		}
		text := "Recent suffix edit"
		if step <= 2 {
			text = strings.Repeat("completed prefix work ", 500)
		}
		if err := emit(provider.StreamEvent{Kind: "text", Text: text}); err != nil {
			return err
		}
		arguments, _ := json.Marshal(map[string]string{"path": "result.txt", "content": fmt.Sprintf("version %d\n", step)})
		return emit(provider.StreamEvent{Kind: "call", Call: &provider.ToolCall{ID: fmt.Sprintf("write-%d", step), Name: "write", Arguments: arguments}})
	}}
	if err := r.Run(&provider.Message{Role: "user", Content: "Second ordinary prompt", InputSource: "queue"}); err != nil {
		t.Fatal(err)
	}
	if step != 4 {
		t.Fatal("missing completed edit cycles", step)
	}
	original := r.Current()
	var baseline int64
	if err := r.Store.DB.QueryRow("SELECT id FROM file_changes WHERE session_id=? ORDER BY id LIMIT 1 OFFSET 1", original).Scan(&baseline); err != nil {
		t.Fatal(err)
	}
	originals := map[string]int64{}
	entries, err := r.Store.Branch(original, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Role == "user" && entry.Kind == "message" {
			var message provider.Message
			if err := json.Unmarshal(entry.Content, &message); err != nil {
				t.Fatal(err)
			}
			if !message.Runtime {
				originals[message.Content] = entry.EventSeq()
			}
		}
	}
	for range 2 {
		if _, err := r.Command("/compact"); err != nil {
			t.Fatal(err)
		}
		entries, err := r.Store.Branch(r.Current(), 0)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, entry := range entries {
			if entry.Role == "user" && entry.Kind == "message" {
				var message provider.Message
				if err := json.Unmarshal(entry.Content, &message); err != nil {
					t.Fatal(err)
				}
				if !message.Runtime {
					count++
					if entry.FileTip != baseline || entry.EventSeq() != originals[message.Content] {
						t.Fatal("human checkpoint crossed the cut baseline or lost identity", entry)
					}
				}
			}
		}
		if count != 4 {
			t.Fatal("four admitted checkpoints did not survive", count)
		}
	}
	assertFile := func(want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(r.Workspace.Root, "result.txt"))
		if err != nil || string(got) != want {
			t.Fatal("filesystem crossed the summarized baseline", string(got), want, err)
		}
	}
	// Co-admitted steers share a restore boundary, as do ordinary checkpoints
	// whose intervening work entered the summary. Exercise both reachable
	// boundaries; all four retained entries above must still use the cut tip.
	for checkpoint := range 2 {
		if _, err := r.Command("/undo"); err != nil {
			t.Fatalf("checkpoint %d: %v", checkpoint, err)
		}
		assertFile("version 2\n")
		if checkpoint == 0 {
			if _, err := r.Command("/redo"); err != nil {
				t.Fatal(err)
			}
			assertFile("version 3\n")
			if _, err := r.Command("/undo"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := r.Command("/undo"); err == nil {
		t.Fatal("undo crossed the compaction floor")
	}
}
