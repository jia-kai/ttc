package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"ttc/internal/historytest"

	"ttc/internal/history"
	"ttc/internal/jobs"
	"ttc/internal/llm"
)

func TestRetainedChildAdmissionFailurePreservesIdleState(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	r.selection.Model.Variants = []string{"none", "alternate"}
	requests := 0
	r.Provider = &childProvider{stream: func(ctx context.Context, request llm.Request, emit func(llm.StreamEvent) error) error {
		requests++
		if request.Selection.Variant != "none" {
			t.Errorf("failed admission changed retained variant: %q", request.Selection.Variant)
		}
		if requests == 2 {
			found := false
			for _, message := range request.Messages {
				found = found || message.Type == llm.TypeAssistant && message.Content == "Retained answer"
			}
			if !found {
				t.Error("resumed child lost retained conversation")
			}
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Retained answer"})
	}}
	turn, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"first","label":"retained"}`)}})
	first := childInvocation(t, r, ids[0], `{"persistent":true,"prompt":"first","label":"retained"}`)
	if first["ok"] != true {
		t.Fatal(first)
	}
	requireChildLifecycle(t, first, "completed", "idle")
	id := first["child_id"].(string)
	_, err := r.Jobs.StartTask(id, jobs.TaskOptions{Kind: "fixture", Background: true}, func(ctx context.Context, _, _ io.Writer) (jobs.TaskResult, error) {
		<-ctx.Done()
		return jobs.TaskResult{}, ctx.Err()
	})
	if err == nil {
		t.Fatal("idle child accepted work outside an assignment")
	}
	r.childStartMu.Lock()
	child := r.children[id]
	beforeView, beforeSelection, beforeAssignment := child.view(), child.selection, child.assignment
	beforeMessages, beforeCursor := append([]llm.Message(nil), child.messages...), child.cursor
	r.childStartMu.Unlock()

	// Restrict the failure to this retained actor's next attempted turn, leaving
	// parent/tool persistence untouched. Drop it before retrying the assignment.
	trigger := fmt.Sprintf(`CREATE TRIGGER reject_retained_turn BEFORE INSERT ON turns WHEN NEW.actor_id='%s' BEGIN SELECT RAISE(ABORT,'synthetic retained admission failure'); END`, strings.ReplaceAll(id, "'", "''"))
	if _, err := historytest.DB(t, r.Store).Exec(trigger); err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal(map[string]any{"persistent": true, "prompt": "rejected followup", "child_id": id})
	if err != nil {
		t.Fatal(err)
	}
	failed := childInvocation(t, r, ids[0], string(args))
	encoded, _ := json.Marshal(failed)
	if failed["ok"] == true || !strings.Contains(string(encoded), "synthetic retained admission failure") {
		t.Fatalf("expected injected admission error: %s", encoded)
	}
	if requests != 1 {
		t.Fatalf("failed admission ran provider: %d requests", requests)
	}
	if live := r.ChildViews("main"); len(live) != 1 || live[0] != beforeView {
		t.Fatalf("failed admission changed live idle view: %+v; want %+v", live, beforeView)
	}
	r.orderMu.Lock()
	published := r.committedChildren("main")
	r.orderMu.Unlock()
	if len(published) != 1 || published[0] != beforeView {
		t.Fatalf("failed admission changed published idle view: %+v; want %+v", published, beforeView)
	}
	r.childStartMu.Lock()
	retained := r.children[id]
	unchanged := retained == child && !retained.closing && retained.assignment == beforeAssignment && reflect.DeepEqual(retained.selection, beforeSelection) && reflect.DeepEqual(retained.messages, beforeMessages) && reflect.DeepEqual(retained.cursor, beforeCursor)
	r.childStartMu.Unlock()
	if !unchanged {
		t.Fatal("failed admission mutated or removed retained context")
	}
	if _, err := historytest.DB(t, r.Store).Exec(`DROP TRIGGER reject_retained_turn`); err != nil {
		t.Fatal(err)
	}
	args, err = json.Marshal(map[string]any{"persistent": true, "prompt": "resume", "child_id": id})
	if err != nil {
		t.Fatal(err)
	}
	resumed := childInvocation(t, r, ids[0], string(args))
	if resumed["ok"] != true || resumed["child_id"] != id || resumed["job_id"] == first["job_id"] || requests != 2 {
		t.Fatal("retained child could not resume", resumed, requests)
	}
	requireChildLifecycle(t, resumed, "completed", "idle")
	closed, err := r.StopChild(context.Background(), "main", id)
	if err != nil {
		t.Fatal(err)
	}
	requireChildLifecycle(t, closed, "completed", "closed")
	if err := r.Store.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
}

func TestStopChildSweepsOwnedJobsAfterLateClose(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	seedRuntime(t, r, "late close fixture")
	const id = "main/child_late_close"
	turn, err := r.Store.BeginChildTurn(r.Current(), id, r.CurrentSelection())
	if err != nil {
		t.Fatal(err)
	}
	owned, err := r.Jobs.StartTask(id, jobs.TaskOptions{Kind: "fixture", Background: true}, func(ctx context.Context, _, _ io.Writer) (jobs.TaskResult, error) {
		<-ctx.Done()
		return jobs.TaskResult{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment := &childAssignment{turn: turn, persistent: true}
	child := &codingChild{id: id, label: "late close", state: "running", turn: turn, assignment: assignment}
	ready := make(chan struct{})
	workerErr := make(chan error, 1)
	job, err := r.Jobs.StartTask(id, jobs.TaskOptions{Kind: "subagent", Background: true}, func(ctx context.Context, _, _ io.Writer) (jobs.TaskResult, error) {
		<-ready
		// Model a successful persistent worker that already decided not to clean
		// up owned jobs. StopChild sets closing after that decision but before
		// terminal publication. Cancellation is a deterministic barrier here,
		// not a cancelled assignment: the answer was already complete.
		<-ctx.Done()
		r.childStartMu.Lock()
		if !child.closing {
			r.childStartMu.Unlock()
			err := fmt.Errorf("StopChild did not set closing before cancelling worker")
			workerErr <- err
			return jobs.TaskResult{}, err
		}
		finish := history.ChildFinish{Type: "child_turn_finished", ChildID: id, TurnID: turn, JobID: assignment.job, Outcome: "completed", ContextState: "closed", Persistent: true}
		seq, err := r.Store.FinishChildTurn(r.Current(), finish)
		if err == nil {
			assignment.finish, assignment.completion = seq, finish
			child.state = "closed"
			delete(r.children, id)
			err = r.publishChild(child.view())
		}
		r.childStartMu.Unlock()
		// Intentionally omit StopOwned: this is the late-close interleaving
		// whose missed worker cleanup StopChild must fence after joining.
		workerErr <- err
		return jobs.TaskResult{}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	r.childStartMu.Lock()
	child.job, assignment.job = job, job
	r.children[id] = child
	err = r.publishChild(child.view())
	r.childStartMu.Unlock()
	close(ready)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := r.StopChild(context.Background(), "main", id)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-workerErr; err != nil {
		t.Fatal(err)
	}
	requireChildLifecycle(t, closed, "completed", "closed")
	if view, err := r.Jobs.View("main", owned); err != nil || view.Status != "cancelled" {
		t.Fatal("StopChild returned without stopping and joining owned job", view, err)
	}
	if len(r.ChildViews("main")) != 0 {
		t.Fatal("closed child remained live")
	}
	r.orderMu.Lock()
	published := r.committedChildren("main")
	r.orderMu.Unlock()
	if len(published) != 0 {
		t.Fatal("closed child remained published", published)
	}
}
