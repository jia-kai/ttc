package tui

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/provider"
)

func TestAttachmentPayloadIsInspectableWithoutConversationExpansion(t *testing.T) {
	p := &provider.Script{Responses: []provider.ScriptResponse{{Text: "Snapshot inspected."}}}
	u := newQuestionTestUI(t, p)
	path := filepath.Join(u.runtime.Workspace.Root, "notes.txt")
	const snapshot = "ATTACHED SNAPSHOT CONTENT"
	if err := os.WriteFile(path, []byte(snapshot), 0600); err != nil {
		t.Fatal(err)
	}
	u.typeText("/attach " + path)
	u.key(tcell.KeyEnter)
	u.wait(t, "Attached")
	if err := os.WriteFile(path, []byte("changed after snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	const authored = "What does this file say?"
	p.Responses[0].Prefix = "user: " + authored + "\n\nAttachment (text): " + path + "\n" + snapshot
	u.typeText(authored)
	u.key(tcell.KeyEnter)
	frame := u.wait(t, "Turn complete")
	if strings.Contains(frame, snapshot) || strings.Contains(frame, "Attachment (text):") {
		t.Fatal("attachment expanded in the conversation", frame)
	}
	var id int64
	if err := u.runtime.Store.DB.QueryRow("SELECT id FROM entries WHERE role='user' AND model_visible=1 ORDER BY id LIMIT 1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	entry, err := u.runtime.Store.Entry(id)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := u.runtime.Store.Inspect(entry)
	if err != nil || !strings.Contains(detail, snapshot) {
		t.Fatal("original attachment disappeared from inspection", detail, err)
	}
	page, err := u.runtime.Store.InspectPage(context.Background(), id, 0, 16384)
	if err != nil || !strings.Contains(page.Text, snapshot) {
		t.Fatal("paged inspection lost canonical input", page, err)
	}
	markdown, err := u.runtime.Store.Transcript(u.runtime.Current(), 0)
	if err != nil || !bytes.Contains(markdown, []byte(snapshot)) {
		t.Fatal("export/compaction archive lost attachment information", err)
	}
	exact, err := u.runtime.Store.TranscriptJSONL(u.runtime.Current(), 0)
	if err != nil || !bytes.Contains(exact, []byte(snapshot)) {
		t.Fatal("exact history lost attachment information", err)
	}
	u.typeText(fmt.Sprintf("/inspect %d", id))
	u.key(tcell.KeyEnter)
	u.wait(t, snapshot)
	u.key(tcell.KeyEscape)
	generation := u.runtime.Generation()
	// A unique name appears only when replay loads session metadata; earlier
	// inspector frames already contained the user label.
	if _, err := u.runtime.Store.DB.Exec("UPDATE sessions SET name=? WHERE id=?", "Reloaded attachment fixture", u.runtime.Current()); err != nil {
		t.Fatal(err)
	}
	u.typeText("/load " + u.runtime.Current())
	u.key(tcell.KeyEnter)
	frame = u.wait(t, "Reloaded attachment fixture")
	if u.runtime.Generation() <= generation || !strings.Contains(frame, "user · "+authored) {
		t.Fatal("did not observe the reloaded conversation", frame)
	}
	if strings.Contains(frame, snapshot) || strings.Contains(frame, "Attachment (text):") {
		t.Fatal("reload expanded attachment payload", frame)
	}
}

func TestAttachedQueueAndSteeringShowAuthoredText(t *testing.T) {
	u, p := gatedComposerUI(t, []provider.ScriptResponse{{Text: "First settled."}, {Text: "Steer settled."}, {Text: "Queued settled."}})
	path := filepath.Join(u.runtime.Workspace.Root, "queued.txt")
	const snapshot = "QUEUED ATTACHMENT PAYLOAD"
	if err := os.WriteFile(path, []byte(snapshot), 0600); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"Queue this request.", "Steer this request."} {
		u.typeText("/attach " + path)
		u.key(tcell.KeyEnter)
		u.wait(t, "Attached")
		u.typeText(text)
		label := "Queued · "
		if i == 0 {
			u.key(tcell.KeyEnter)
		} else {
			u.screen.PostEventWait(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModAlt))
			label = "Steer · "
		}
		frame := u.wait(t, label+text)
		if strings.Contains(frame, snapshot) || strings.Contains(frame, "Attachment (text):") {
			t.Fatal("pending input expanded attachments", frame)
		}
	}
	p.Responses[1].Prefix = "user: Steer this request.\n\nAttachment (text): " + path + "\n" + snapshot
	p.Responses[2].Prefix = "user: Queue this request.\n\nAttachment (text): " + path + "\n" + snapshot
	close(p.release)
	frame := u.wait(t, "Queued settled.")
	if strings.Contains(frame, snapshot) || strings.Contains(frame, "Attachment (text):") {
		t.Fatal("admitted queued/steered messages expanded attachments", frame)
	}
}
