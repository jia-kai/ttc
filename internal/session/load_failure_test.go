package session

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
	"ttc/internal/historytest"

	"ttc/internal/history"
	"ttc/internal/jobs"
	"ttc/internal/llm"
)

func TestFailedHistoryLoadPreservesActiveRuntimeAndTargetMetadata(t *testing.T) {
	for _, failure := range []string{"empty call ID", "duplicate call ID"} {
		t.Run(failure, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			seedRuntime(t, r, "Active research")
			current := r.Current()
			targetID := history.NewID("session")
			selection := r.CurrentSelection()
			selection.Model.ID = "other-model"
			turn, _, err := r.Store.StartSession(targetID, r.Workspace.Root, selection, llm.Message{Type: llm.TypeUserInput, Content: "Archived research"})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Store.FinishTurn(turn, "completed"); err != nil {
				t.Fatal(err)
			}
			retained, err := r.Store.Append(targetID, "", "main", "message", "user", true, llm.Message{Type: llm.TypeUserInput, Content: "Continue research"})
			if err != nil {
				t.Fatal(err)
			}
			target, err := r.Store.Continue(targetID, "Research handoff", retained, retained, nil, time.Now(), nil)
			if err != nil {
				t.Fatal(err)
			}
			prompt, err := r.Store.RecordSystemPrompt(target.ID, "", "main", 0, "Old instructions")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := historytest.DB(t, r.Store).Exec("UPDATE entries SET created_ms=? WHERE id=?", time.Now().Add(-2*time.Hour).UnixMilli(), prompt); err != nil {
				t.Fatal(err)
			}
			// Invalid durable tool exchanges must still reject the load before
			// replacing live state, independently of disposable export files.
			calls := []llm.ToolCall{{ID: "", Name: "read"}}
			if failure == "duplicate call ID" {
				calls = []llm.ToolCall{{ID: "duplicate", Name: "read"}, {ID: "duplicate", Name: "read"}}
			}
			if _, err := r.Store.Append(target.ID, "", "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Calls: calls}); err != nil {
				t.Fatal(err)
			}
			target, err = r.Store.Session(target.ID)
			if err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			if _, err := r.Jobs.StartTask("main", jobs.TaskOptions{Kind: "review", Label: "Keep running", Background: true}, func(ctx context.Context, _, _ io.Writer) (jobs.TaskResult, error) {
				close(entered)
				<-ctx.Done()
				return jobs.TaskResult{}, ctx.Err()
			}); err != nil {
				t.Fatal(err)
			}
			<-entered
			// A pending main question and image click are live
			// state; a rejected session switch must not discard either handle.
			r.questions.mu.Lock()
			r.questions.pending = &questionForm{view: QuestionForm{ID: "question", Questions: []Question{{ID: "answer", Prompt: "Choose?"}}}, reply: make(chan questionReply, 1)}
			r.questions.mu.Unlock()
			r.images.mu.Lock()
			r.images.pending["click"] = pendingImage{view: ImageSnapshot{ID: "click", Actor: "main", Width: 8, Height: 8}, reply: make(chan llm.Message, 1)}
			r.images.actors["main"] = "click"
			r.images.mu.Unlock()
			jobs, generation := r.Jobs, r.Generation()
			if _, err := r.Command("/load " + target.ID); err == nil || !strings.Contains(err.Error(), "invalid tool exchange") {
				t.Fatal("invalid history accepted", err)
			}
			if r.Current() != current || r.Jobs != jobs || r.Generation() != generation || len(r.Jobs.Live()) != 1 || r.PendingQuestion() == nil || !r.ImageClickPending("click") {
				t.Fatal("failed load changed active runtime", r.Current(), r.Generation(), r.Jobs.Live(), r.PendingQuestion(), r.ImageClickPending("click"))
			}
			after, err := r.Store.Session(target.ID)
			if err != nil || after.EntryTip != target.EntryTip || after.Model.Model.ID != selection.Model.ID {
				t.Fatal("failed preflight changed target instructions or model", after, err)
			}
		})
	}
}

func TestLoadContinuationWithoutArchiveFiles(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Earlier research evidence")
	before := r.Current()
	retained, err := r.Store.Append(before, "", "main", "message", "user", true, llm.Message{Type: llm.TypeUserInput, Content: "Continue research"})
	if err != nil {
		t.Fatal(err)
	}
	continued, err := r.Store.Continue(before, "Research handoff", retained, retained, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// No transcript fixture has been generated: SQLite alone must load the
	// handoff and retain access to the evidence replaced by its summary.
	if _, err := r.Command("/load " + continued.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := r.CurrentSession()
	if err != nil {
		t.Fatal(err)
	}
	text, err := r.Store.ActorTranscriptJSONL(loaded.ID, "main", loaded.EntryTip)
	if err != nil || !strings.Contains(string(text), "Earlier research evidence") || !strings.Contains(string(text), "Continue research") {
		t.Fatal("loaded continuation lost on-demand ancestry", err, string(text))
	}
}
