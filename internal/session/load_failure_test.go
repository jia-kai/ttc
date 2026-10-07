package session

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"ttc/internal/history"
	"ttc/internal/llm"
)

func TestFailedArchiveLoadPreservesActiveRuntimeAndTargetMetadata(t *testing.T) {
	for _, failure := range []string{"missing", "corrupt"} {
		t.Run(failure, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			seedRuntime(t, r, "Active research")
			current := r.Current()
			targetID := history.NewID("session")
			selection := r.CurrentSelection()
			selection.Model.ID = "other-model"
			turn, _, err := r.Store.StartSession(targetID, r.Workspace.Root, selection, llm.Message{Role: "user", Content: "Archived research"})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Store.FinishTurn(turn, "completed"); err != nil {
				t.Fatal(err)
			}
			retained, err := r.Store.Append(targetID, "", "main", "message", "user", true, llm.Message{Role: "user", Content: "Continue research"})
			if err != nil {
				t.Fatal(err)
			}
			archive, err := r.Store.ArchiveTranscript(targetID, 0)
			if err != nil {
				t.Fatal(err)
			}
			target, err := r.Store.Continue(targetID, "Research handoff", archive, retained, nil, time.Now(), nil)
			if err != nil {
				t.Fatal(err)
			}
			prompt, err := r.Store.RecordSystemPrompt(target.ID, "", "main", 0, "Old instructions")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Store.DB.Exec("UPDATE entries SET created_ms=? WHERE id=?", time.Now().Add(-2*time.Hour).UnixMilli(), prompt); err != nil {
				t.Fatal(err)
			}
			target, err = r.Store.Session(target.ID)
			if err != nil {
				t.Fatal(err)
			}
			if failure == "missing" {
				err = os.Remove(archive + ".jsonl")
			} else {
				err = os.WriteFile(archive+".jsonl", []byte("Broken exact archive"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			if _, err := r.Jobs.StartTask("main", "review", "Keep running", true, false, func(ctx context.Context, _, _ io.Writer) error {
				close(entered)
				<-ctx.Done()
				return ctx.Err()
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
			if _, err := r.Command("/load " + target.ID); err == nil || !strings.Contains(err.Error(), "compaction archive") {
				t.Fatal("broken archive accepted", err)
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
