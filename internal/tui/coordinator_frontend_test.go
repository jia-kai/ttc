package tui

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v3"
	contextbuild "ttc/internal/context"
	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/session"
	"ttc/internal/skills"
	"ttc/internal/tool"
	"ttc/internal/workspace"
)

func coordinatorFrontendFixture(t *testing.T) *frontendController {
	t.Helper()
	store, err := history.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	w, err := workspace.Open(t.TempDir(), store)
	if err != nil {
		t.Fatal(err)
	}
	r, err := session.New(context.Background(), store, w, &llm.Script{}, llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}, "", &skills.Catalog{}, tool.WebSearchConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.AutoName = false
	t.Cleanup(r.Close)
	return &frontendController{frontend: &Frontend{Runtime: r, Output: io.Discard}, view: newTranscript(), draft: newComposer(""), completion: &completionMenu{}, observedGeneration: r.Generation(), queueGeneration: r.Generation()}
}

func TestCoordinatorLateRestorePreservesNewerDraftAndAttachments(t *testing.T) {
	c := coordinatorFrontendFixture(t)
	c.draft.set("newer typing")
	c.attachments = []contextbuild.Attachment{{Path: "new.txt", Kind: "text", Text: "new snapshot"}}
	returned := contextbuild.Input{Text: "old rejected input", Attachments: []contextbuild.Attachment{{Path: "old.txt", Kind: "text", Text: "old immutable snapshot"}}}
	if err := c.handleUpdate(session.Update{Result: &session.OperationResult{Kind: session.RequestInput, Restored: &returned}}); err != nil {
		t.Fatal(err)
	}
	if c.draft.text != "newer typing\n\nold rejected input" {
		t.Fatal("late rejection overwrote newer draft", c.draft.text)
	}
	if len(c.attachments) != 2 || c.attachments[0].Text != "new snapshot" || c.attachments[1].Text != "old immutable snapshot" {
		t.Fatal("late rejection replaced attachment ownership", c.attachments)
	}
}

func TestCoordinatorEnterCapturesRenderedIdentityNotUnseenProjection(t *testing.T) {
	c := coordinatorFrontendFixture(t)
	c.operationID = 23 // Received but not drawn.
	c.observedOperationID = 11
	c.draft.set("guidance for displayed turn")
	next := c.handleComposerKey(tcell.NewEventKey(tcell.KeyEnter, "", 0))
	if !next.ready || !next.steering || next.operationID != 11 || next.generation != c.observedGeneration {
		t.Fatal("Enter retargeted an unseen execution", next)
	}
	c.operationID = 24
	c.busy = true
	c.observedOperationID = 0
	c.draft.set("displayed idle input")
	next = c.handleComposerKey(tcell.NewEventKey(tcell.KeyEnter, "", 0))
	if next.steering || next.operationID != 0 {
		t.Fatal("unseen busy state converted idle input to steering", next)
	}
}

func TestCoordinatorOldControlResultCannotClearReplacementProjection(t *testing.T) {
	c := coordinatorFrontendFixture(t)
	c.operationID = 29
	c.busy = true
	if err := c.handleUpdate(session.Update{Result: &session.OperationResult{OperationID: 11, Generation: c.queueGeneration, Kind: session.RequestReconnect, Text: "Connections reset"}}); err != nil {
		t.Fatal(err)
	}
	if !c.busy || c.operationID != 29 {
		t.Fatal("old result cleared newer execution projection", c.busy, c.operationID)
	}
	if !strings.Contains(c.view.lines[len(c.view.lines)-1].text, "Connections reset") {
		t.Fatal("control result was not projected")
	}
}

func TestCoordinatorExplicitCancelReplacesOldDraftButPreservesNewEdits(t *testing.T) {
	for _, newer := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "newer draft"}[newer], func(t *testing.T) {
			c := coordinatorFrontendFixture(t)
			c.attachments = []contextbuild.Attachment{{Path: "discard.txt", Kind: "text", Text: "pre-command draft"}}
			c.pendingRestores = map[uint64]restoreBaseline{17: {draftRevision: c.draft.revision, attachmentEpoch: c.attachmentEpoch, attachmentCount: 1}}
			if newer {
				c.draft.insert("new typing")
				c.attachments = append(c.attachments, contextbuild.Attachment{Path: "new.txt", Kind: "text", Text: "new snapshot"})
			}
			original := contextbuild.Input{Text: "original queued prompt", Attachments: []contextbuild.Attachment{{Path: "old.txt", Kind: "text", Text: "original snapshot"}}}
			if err := c.handleUpdate(session.Update{Result: &session.OperationResult{RequestID: 17, Kind: session.RequestCancelQueue, Restored: &original}}); err != nil {
				t.Fatal(err)
			}
			wantText := original.Text
			wantAttachments := 1
			if newer {
				wantText = "new typing\n\n" + wantText
				wantAttachments = 2
			}
			if c.draft.text != wantText || len(c.attachments) != wantAttachments || c.attachments[0].Text != "original snapshot" {
				t.Fatal("explicit cancel lost replacement/new-edit ownership", c.draft.text, c.attachments)
			}
			if newer && c.attachments[1].Text != "new snapshot" {
				t.Fatal("cancel discarded a post-submit attachment", c.attachments)
			}
		})
	}
}
