package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"ttc/internal/history"
	"ttc/internal/historytest"
	"ttc/internal/llm"
	"ttc/internal/render"
)

func TestFinishChildPersistsInspectableMetadataBeforeJobCompletion(t *testing.T) {
	for _, easy := range []bool{false, true} {
		t.Run(fmt.Sprint(easy), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			seedRuntime(t, r, "Saved completion metadata fixture")
			childID := "main/child_saved_metadata"
			turn, err := r.Store.BeginChildTurn(r.Current(), childID, r.selection)
			if err != nil {
				t.Fatal(err)
			}
			response, err := r.Store.Append(r.Current(), turn, childID, "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: "# Saved answer\n\n- first item"})
			if err != nil {
				t.Fatal(err)
			}
			label := strings.Repeat("界", 600)
			assignment := &childAssignment{turn: turn, job: "saved-job", result: response, answer: "preview", background: true}
			child := &codingChild{id: childID, label: label, easy: easy, state: "running", turn: turn, job: "saved-job", assignment: assignment}
			if err := r.finishChild(child, childTask{actor: childID, turn: turn, child: child, assignment: assignment}, nil); err != nil {
				t.Fatal(err)
			}
			var terminalJobs int
			if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM entries WHERE json_extract(content_json,'$.type')='job_completion'`).Scan(&terminalJobs); err != nil || terminalJobs != 0 {
				t.Fatal("fixture must inspect before the later job callback", terminalJobs, err)
			}
			entry, err := r.Store.Entry(assignment.finish)
			if err != nil {
				t.Fatal(err)
			}
			var finish history.ChildFinish
			if err := json.Unmarshal(entry.Content, &finish); err != nil || finish.Easy == nil || *finish.Easy != easy || finish.Label != render.ResponsePrefix(label) || len(finish.Label) > 1024 {
				t.Fatalf("completion lost bounded explicit metadata: %+v, %v", finish, err)
			}
			wire := childAPIObject(t, childCompletionNotification(finish))
			if wire["label"] != nil || wire["easy"] != nil {
				t.Fatalf("saved UI metadata leaked into the unchanged notification API: %v", wire)
			}
			body, err := json.Marshal(childCompletionNotification(finish))
			if err != nil {
				t.Fatal(err)
			}
			id, err := r.Store.Append(r.Current(), "", "main", "message", "user", true, llm.Message{Type: llm.TypeRuntimeNotification, Content: string(body), EventSeq: assignment.finish})
			if err != nil {
				t.Fatal(err)
			}
			// Subsequent mutable actor state must not influence this saved row.
			child.label, child.easy = "Later reused label", !easy
			answer, err := r.Store.ReadChildAnswer(context.Background(), id)
			if err != nil || answer.ResponseID != response || answer.Label != render.ResponsePrefix(label) || answer.Easy == nil || *answer.Easy != easy {
				t.Fatalf("immediate saved answer metadata missing: %+v, %v", answer, err)
			}
		})
	}
}
