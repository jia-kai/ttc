package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
)

func TestSteeringBoundaryOwnsSeparateUndoCheckpoint(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	requests := 0
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		requests++
		if requests == 1 {
			if err := r.Steer(contextbuild.Input{Text: "Steer: revise after first write"}); err != nil {
				return err
			}
			for _, m := range req.Messages {
				if strings.HasPrefix(m.Content, "Steer:") {
					t.Fatal("steer leaked into active snapshot")
				}
			}
		} else {
			found := false
			for _, m := range req.Messages {
				if m.Role == "user" && m.Content == "Steer: revise after first write" {
					found = true
				}
			}
			if !found {
				t.Fatal("steer missing at settled boundary")
			}
		}
		if requests == 3 {
			return emit(llm.StreamEvent{Kind: "text", Text: "Done"})
		}
		content := "first\n"
		if requests == 2 {
			content = "revised\n"
		}
		arguments, _ := json.Marshal(map[string]string{"path": "result.txt", "content": content})
		return emit(llm.StreamEvent{Kind: "call", Call: &llm.ToolCall{ID: content, Name: "write", Arguments: arguments}})
	}}
	if err := r.Run(&llm.Message{Role: "user", Content: "Write the result"}); err != nil {
		t.Fatal(err)
	}
	if count, _ := r.SteeringPreview(0); requests != 3 || count != 0 {
		t.Fatal(requests, count)
	}
	var human, steer int
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM turns WHERE trigger='user'").Scan(&human); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM turns WHERE trigger='steer'").Scan(&steer); err != nil {
		t.Fatal(err)
	}
	if human != 1 || steer != 1 {
		t.Fatal("steering ended/replaced main turn", human, steer)
	}
	if _, err := r.Command("/undo"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(r.Workspace.Root, "result.txt"))
	if err != nil || string(data) != "first\n" {
		t.Fatal("steer undo lost preceding settled write", string(data), err)
	}
	if _, err := r.Command("/redo"); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(r.Workspace.Root, "result.txt"))
	if err != nil || string(data) != "revised\n" {
		t.Fatal("steer redo lost later write", string(data), err)
	}
}

func TestSteeringRejectedAdmissionRetainsInputAndCheckpoint(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Initial instruction")
	r.mu.Lock()
	r.activeTurn = "test-running"
	_, r.activeCancel = context.WithCancel(r.ctx)
	r.mu.Unlock()
	if err := r.Steer(contextbuild.Input{Text: "Queued human steer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.DB.Exec(`CREATE TRIGGER reject_steer_request BEFORE INSERT ON model_requests BEGIN SELECT RAISE(ABORT,'rejected request'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.admitMain(context.Background(), "", r.selection); err == nil {
		t.Fatal("rejected request accepted")
	}
	var checkpoints int
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM turns WHERE trigger='steer'").Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if count, _ := r.SteeringPreview(0); checkpoints != 0 || count != 1 {
		t.Fatal("admission failure consumed steering", checkpoints, count)
	}
	if _, err := r.Store.DB.Exec("DROP TRIGGER reject_steer_request"); err != nil {
		t.Fatal(err)
	}
	admitted, _, err := r.admitMain(context.Background(), "", r.selection)
	count, _ := r.SteeringPreview(0)
	if err != nil || len(admitted.SteerEntries) != 1 || count != 0 {
		t.Fatal(admitted, err)
	}
}
