package history

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"scicode/internal/provider"
	"scicode/internal/render"
)

func TestConcurrentStartupAndIndependentSessionCommits(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shared ? data")
	work := t.TempDir()
	const instances = 8
	start := make(chan struct{})
	errors := make(chan error, instances)
	var wg sync.WaitGroup
	for i := range instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s, err := Open(root)
			if err != nil {
				errors <- err
				return
			}
			defer s.Close()
			id := NewID("session")
			model := provider.Selection{Provider: fmt.Sprintf("provider-%d", i), Model: provider.ScriptModel(), Variant: "none"}
			turn, _, err := s.StartSession(id, work, model, provider.Message{Role: "user", Content: fmt.Sprint(i)})
			if err == nil {
				for j := 0; j < 10; j++ {
					_, err = s.Append(id, turn, "main", "message", "assistant", true, provider.Message{Role: "assistant", Content: fmt.Sprint(j)})
					if err != nil {
						break
					}
				}
			}
			if err == nil {
				err = s.FinishTurn(turn, "completed")
			}
			if err == nil {
				err = s.SaveSelection(model)
			}
			errors <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	list, err := s.Sessions(work)
	if err != nil || len(list) != instances {
		t.Fatal(list, err)
	}
	for _, session := range list {
		messages, err := s.Messages(session.ID)
		if err != nil || len(messages) != 11 {
			t.Fatal(messages, err)
		}
		choice, err := s.LastSelection(session.Model.Provider)
		if err != nil || choice == nil {
			t.Fatal(choice, err)
		}
	}
}

func TestOpeningSharedHistoryDoesNotSettleLiveOrCrashedWork(t *testing.T) {
	s, source, turn, request := historyFixture(t)
	call, err := s.CallIntent(source.ID, turn, "main", request, provider.ToolCall{ID: "pending", Name: "shell", Arguments: []byte(`{"command":"never rerun"}`)})
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(s.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var status string
	var result sql.NullString
	if err := other.DB.QueryRow("SELECT status FROM turns WHERE id=?", turn).Scan(&status); err != nil || status != "running" {
		t.Fatal(status, err)
	}
	if err := other.DB.QueryRow("SELECT status FROM model_requests WHERE id=?", request).Scan(&status); err != nil || status != "running" {
		t.Fatal(status, err)
	}
	if err := other.DB.QueryRow("SELECT result_json FROM tool_calls WHERE id=?", call).Scan(&result); err != nil || result.Valid {
		t.Fatal(result, err)
	}
}

func TestManualLoadsSnapshotBalancedHistoryAndRemainIndependent(t *testing.T) {
	s, source, turn, request := historyFixture(t)
	call := provider.ToolCall{ID: "done", Name: "read", Arguments: []byte(`{"path":"file"}`)}
	_, ids, err := s.Assistant(source.ID, turn, "main", request, provider.Message{Role: "assistant", Calls: []provider.ToolCall{call}})
	if err != nil {
		t.Fatal(err)
	}
	result := []byte(`{"content":"saved"}`)
	md := render.Tool(call.Name, call.Arguments, result)
	if _, err := s.CallResult(source.ID, turn, "main", ids[0], result, map[string]string{"name": "read"}, md, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(source.ID, turn, "main", "message", "developer", true, provider.Message{Role: "developer", Runtime: true, Content: "old live jobs"}); err != nil {
		t.Fatal(err)
	}
	want, err := s.Messages(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	want = want[:len(want)-1] // The new runtime supplies its own environment.
	call.ID = "unfinished"
	if _, _, err := s.Assistant(source.ID, turn, "main", request, provider.Message{Role: "assistant", Content: "incomplete", Calls: []provider.ToolCall{call}}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Session(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Load(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Load(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID || a.ID == source.ID || a.LineageID != source.LineageID || a.FileTip != 0 || a.UndoFloor != a.EntryTip {
		t.Fatal(a, b)
	}
	for _, loaded := range []Session{a, b} {
		messages, err := s.Messages(loaded.ID)
		if err != nil || !reflect.DeepEqual(messages, want) {
			t.Fatal(messages, want, err)
		}
		if _, err := s.UndoTarget(loaded.ID); err == nil {
			t.Fatal("imported work is undoable")
		}
		entries, err := s.Branch(loaded.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Kind == "tool_result" && s.Label(entry) != md.Summary {
				t.Fatal("copy lost its presentation", entry)
			}
		}
	}
	newTurn, _, err := s.AdmitTurn(a.ID, "user", a.Model, &provider.Message{Role: "user", Content: "independent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTurn(newTurn, "completed"); err != nil {
		t.Fatal(err)
	}
	archive, err := s.ArchiveTranscript(a.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.Continue(a.ID, "copy handoff", archive, a.UndoFloor, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UndoTarget(next.ID); err != nil {
		t.Fatal("copy compaction lost new undo", err)
	}
	after, err := s.Session(source.ID)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatal("loading changed source", before, after, err)
	}
	var owner string
	if err := s.DB.QueryRow("SELECT session_id FROM turns WHERE id=?", turn).Scan(&owner); err != nil || owner != source.ID {
		t.Fatal("copy compaction moved source turn", owner, err)
	}
	bMessages, err := s.Messages(b.ID)
	if err != nil || !reflect.DeepEqual(bMessages, want) {
		t.Fatal("sibling changed", bMessages, err)
	}
	if _, err := s.Cleanup(context.Background(), time.Now(), next.ID); err != nil {
		t.Fatal(err)
	}
}
