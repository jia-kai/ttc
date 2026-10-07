package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func assertStoredSessions(t *testing.T, r *Runtime, want int) {
	t.Helper()
	var count int
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM sessions").Scan(&count); err != nil || count != want {
		t.Fatal("stored sessions", count, want, err)
	}
}

func TestBlankLifecyclePersistsOnlyFirstMessageAndKeepsIdentity(t *testing.T) {
	r, _ := runtimeFixture(t, []llm.ScriptResponse{{Text: "First answer"}, {Text: "Second answer"}})
	initial := r.Current()
	for _, command := range []string{"/help", "/sessions", "/new", "/clear"} {
		if _, err := r.Command(command); err != nil {
			t.Fatal(command, err)
		}
		assertStoredSessions(t, r, 0)
	}
	if initial == r.Current() {
		t.Fatal("new/clear did not replace the blank identity")
	}
	for _, command := range []string{"/undo", "/redo", "/compact", "/export " + filepath.Join(t.TempDir(), "empty.md")} {
		if _, err := r.Command(command); err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatal(command, err)
		}
	}
	if err := r.Run(nil); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatal("blank async turn", err)
	}
	next := r.CurrentSelection()
	next.Model.ID = "chosen-before-message"
	if err := r.RequestModel(next); err != nil {
		t.Fatal(err)
	}
	event, err := r.ApplyModel("")
	if err != nil || event.Kind != "status" || event.EntryID != 0 {
		t.Fatal(event, err)
	}
	assertStoredSessions(t, r, 0)
	metadata, err := r.CurrentSession()
	if err != nil || metadata.Model.Model.ID != next.Model.ID || metadata.Name != "New session" {
		t.Fatal(metadata, err)
	}
	if entries, err := r.Entries(); err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
	choice, err := r.Store.LastSelection(next.Provider)
	if err != nil || choice == nil || choice.Model.ID != next.Model.ID {
		t.Fatal(choice, err)
	}
	id := r.Current()
	if err := r.Run(&llm.Message{Role: "user", Content: "First submission"}); err != nil {
		t.Fatal(err)
	}
	assertStoredSessions(t, r, 1)
	if id != r.Current() {
		t.Fatal("first save changed conversation identity")
	}
	if _, err := r.Command("/undo"); err != nil {
		t.Fatal(err)
	}
	undone, err := r.CurrentSession()
	if err != nil || undone.EntryTip != 0 || undone.RedoTip == 0 {
		t.Fatal("first turn undo did not reach empty history", undone, err)
	}
	if _, err := r.Command("/redo"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Command("/new"); err != nil {
		t.Fatal(err)
	}
	assertStoredSessions(t, r, 1)
	if r.Current() == id {
		t.Fatal("new retained old identity")
	}
	if _, err := r.Command("/load " + id); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(&llm.Message{Role: "user", Content: "Second submission"}); err != nil {
		t.Fatal(err)
	}
	var users int
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM entries WHERE session_id=? AND kind='message' AND role='user'", r.Current()).Scan(&users); err != nil || users != 2 {
		t.Fatal("first message duplicated", users, err)
	}
}

func TestBlankFirstSaveFailurePreservesIdentityAndCanRetry(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	called := 0
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		called++
		if req.ConversationID != r.Current() {
			t.Fatal("request identity differs from saved session")
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "saved"})
	}}
	id := r.Current()
	if _, err := r.Store.DB.Exec(`CREATE TRIGGER reject_first BEFORE INSERT ON entries BEGIN SELECT RAISE(ABORT,'injected first save failure'); END`); err != nil {
		t.Fatal(err)
	}
	message := &llm.Message{Role: "user", Content: "Keep this first submission"}
	if err := r.Run(message); err == nil || !strings.Contains(err.Error(), "save first message") {
		t.Fatal(err)
	}
	assertStoredSessions(t, r, 0)
	if called != 0 || r.Current() != id || r.persisted {
		t.Fatal("failed save started inference or changed draft", called, r.Current())
	}
	if _, err := r.Store.DB.Exec("DROP TRIGGER reject_first"); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(message); err != nil || called != 1 || r.Current() != id {
		t.Fatal(called, err)
	}
	assertStoredSessions(t, r, 1)
}

func TestBlankPreferenceFailureDoesNotQueueModel(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	before := r.CurrentSelection()
	next := before
	next.Model.ID = "new-choice"
	if err := os.WriteFile(filepath.Join(r.Store.Root, "model-choices.json"), []byte("invalid JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.RequestModel(next); err == nil {
		t.Fatal("ignored preference failure")
	}
	if event, err := r.ApplyModel(""); err != nil || event.Kind != "" || r.CurrentSelection().Model.ID != before.Model.ID {
		t.Fatal(event, err)
	}
	assertStoredSessions(t, r, 0)
}
