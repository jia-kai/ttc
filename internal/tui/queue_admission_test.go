package tui

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	contextbuild "scicode/internal/context"
	"scicode/internal/provider"
	"scicode/internal/session"
)

func holdQueuedAdmission(t *testing.T, r *session.Runtime) func() {
	t.Helper()
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- r.Workspace.Admit(context.Background(), func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	var once sync.Once
	unlock := func() {
		once.Do(func() {
			close(release)
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(unlock)
	return unlock
}

func assertNoQueuedInputHistory(t *testing.T, u *questionTestUI) {
	t.Helper()
	for _, query := range []string{
		"SELECT count(*) FROM sessions",
		"SELECT count(*) FROM turns",
		"SELECT count(*) FROM entries WHERE role='user' AND model_visible=1",
		"SELECT count(*) FROM model_requests",
	} {
		var count int
		if err := u.runtime.Store.DB.QueryRow(query).Scan(&count); err != nil || count != 0 {
			t.Fatal("cancelled initial input left history", query, count, err)
		}
	}
}

func TestCancelQueueBeforeInitialAdmissionRestoresSnapshots(t *testing.T) {
	p := &cancelInputProvider{Script: provider.Script{Responses: []provider.ScriptResponse{{Text: "Restored admitted."}}}, requests: make(chan provider.Request, 8), release: make(chan struct{})}
	close(p.release)
	u := newQuestionTestUI(t, p)
	path := filepath.Join(u.runtime.Workspace.Root, "original.txt")
	if err := os.WriteFile(path, []byte("snapshot before admission"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := contextbuild.Snapshot(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	u.typeText("/attach " + path)
	u.key(tcell.KeyEnter)
	u.wait(t, "1 attachments")
	unlock := holdQueuedAdmission(t, u.runtime)
	original := "  original\n\tλ queued input  "
	pasteCancelInput(u, original)
	u.key(tcell.KeyEnter)
	u.wait(t, "Working")
	assertNoQueuedInputHistory(t, u)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	u.typeText("/cancel-queue")
	u.key(tcell.KeyEnter)
	frame := u.wait(t, "Cancelled pending input")
	if !strings.Contains(frame, "λ queued input") || !strings.Contains(frame, "1 attachments") {
		t.Fatal("in-flight unadmitted input did not restore composer", frame)
	}
	unlock()
	assertNoQueuedInputHistory(t, u)
	// Resubmission must reuse the original snapshot after its path is gone.
	releaseRestored := holdQueuedAdmission(t, u.runtime)
	u.key(tcell.KeyEnter)
	u.wait(t, "Working")
	time.Sleep(30 * time.Millisecond)
	releaseRestored()
	u.wait(t, "Restored admitted.")
	req := cancelTestRequest(t, p)
	want := (contextbuild.Input{Text: original, Attachments: []contextbuild.Attachment{snapshot}}).Message()
	got := latestHumanInput(req)
	want.InputSource = "normal"
	want.InputTimeMS = originalInputCommitTime(t, u, want.Content)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("in-flight restoration changed snapshot or original text", got, want)
	}
}

func TestCancelQueueNewerQueuedInputPrecedesAdmissionTicket(t *testing.T) {
	u := newQuestionTestUI(t, &provider.Script{})
	unlock := holdQueuedAdmission(t, u.runtime)
	u.typeText("oldest waiting for admission")
	u.key(tcell.KeyEnter)
	u.wait(t, "Working")
	u.typeText("newer queued input")
	u.key(tcell.KeyEnter)
	u.wait(t, "Queued · newer queued input")
	u.typeText("/cancel-queue")
	u.key(tcell.KeyEnter)
	u.wait(t, "> newer queued input")
	u.key(tcell.KeyCtrlA)
	u.key(tcell.KeyCtrlK)
	u.typeText("/cancel-queue")
	u.key(tcell.KeyEnter)
	u.wait(t, "> oldest waiting for admission")
	u.key(tcell.KeyCtrlA)
	u.key(tcell.KeyCtrlK)
	unlock()
	u.typeText("/cancel-queue")
	u.key(tcell.KeyEnter)
	u.wait(t, "no queued prompt to cancel")
	assertNoQueuedInputHistory(t, u)
}

func TestCancelQueueDoesNotInterruptAdmittedInput(t *testing.T) {
	u, p := newCancelInputUI(t, []provider.ScriptResponse{{Text: "Active turn completed."}})
	u.typeText("/cancel-queue")
	u.key(tcell.KeyEnter)
	u.wait(t, "no queued prompt to cancel")
	close(p.release)
	u.wait(t, "Active turn completed.")
	u.wait(t, "Turn complete")
	var status string
	if err := u.runtime.Store.DB.QueryRow("SELECT status FROM turns").Scan(&status); err != nil || status != "completed" {
		t.Fatal("queue cancellation interrupted admitted turn", status, err)
	}
}
