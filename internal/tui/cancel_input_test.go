package tui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	contextbuild "ttc/internal/context"
	"ttc/internal/provider"
	"ttc/internal/session"
)

// cancelInputProvider gates the first response while the UI modifies pending
// inputs and records the exact frozen request received at each admission.
type cancelInputProvider struct {
	provider.Script
	requests chan provider.Request
	release  chan struct{}
	first    bool
}

func (p *cancelInputProvider) Stream(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
	select {
	case p.requests <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	if !p.first {
		p.first = true
		select {
		case <-p.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.Script.Stream(ctx, req, emit)
}

func cancelTestRequest(t *testing.T, p *cancelInputProvider) provider.Request {
	t.Helper()
	select {
	case req := <-p.requests:
		return req
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
		return provider.Request{}
	}
}

func newCancelInputUI(t *testing.T, responses []provider.ScriptResponse) (*questionTestUI, *cancelInputProvider) {
	t.Helper()
	p := &cancelInputProvider{Script: provider.Script{Responses: responses}, requests: make(chan provider.Request, 8), release: make(chan struct{})}
	u := newQuestionTestUIWithSetup(t, p, nil, func(r *session.Runtime) {
		selection := r.CurrentSelection()
		selection.Model.ID = "cancel-image-fixture"
		selection.Model.Images = true
		if err := r.RequestModel(selection); err != nil {
			t.Fatal(err)
		}
	})
	u.typeText("initial")
	u.key(tcell.KeyEnter)
	cancelTestRequest(t, p)
	return u, p
}

func pasteCancelInput(u *questionTestUI, text string) {
	u.screen.PostEventWait(tcell.NewEventPaste(true))
	for _, r := range text {
		switch r {
		case '\n':
			u.key(tcell.KeyEnter)
		case '\t':
			u.key(tcell.KeyTab)
		default:
			u.screen.PostEventWait(tcell.NewEventKey(tcell.KeyRune, r, 0))
		}
	}
	u.screen.PostEventWait(tcell.NewEventPaste(false))
}

func submitCancelInput(u *questionTestUI, steer bool) {
	if steer {
		u.key(tcell.KeyEnter)
	} else {
		u.screen.PostEventWait(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModAlt))
	}
}

func TestCancelPendingInputRestoresFullTextAndAttachmentSnapshots(t *testing.T) {
	for _, steer := range []bool{false, true} {
		name, command, preview := "queue", "/cancel-queue", "Queued · "
		if steer {
			name, command, preview = "steer", "/cancel-steer", "Steer · "
		}
		t.Run(name, func(t *testing.T) {
			responses := []provider.ScriptResponse{{Text: "Initial settled."}, {Text: "Older settled."}, {Text: "Restored settled."}}
			if steer {
				responses = responses[:2]
				responses[1].Text = "Restored settled."
			}
			u, p := newCancelInputUI(t, responses)
			pasteCancelInput(u, "older pending input")
			submitCancelInput(u, steer)
			u.wait(t, preview+"older pending input")
			textPath := filepath.Join(u.runtime.Workspace.Root, "snapshot.txt")
			dirPath := filepath.Join(u.runtime.Workspace.Root, "directory")
			imagePath := filepath.Join(u.runtime.Workspace.Root, "image.png")
			if err := os.WriteFile(textPath, []byte("original snapshot"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(dirPath, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dirPath, "original.txt"), []byte("entry"), 0600); err != nil {
				t.Fatal(err)
			}
			image, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(imagePath, image, 0600); err != nil {
				t.Fatal(err)
			}
			var snapshots []contextbuild.Attachment
			for _, path := range []string{textPath, dirPath, imagePath} {
				a, err := contextbuild.Snapshot(context.Background(), path, true)
				if err != nil {
					t.Fatal(err)
				}
				snapshots = append(snapshots, a)
				u.typeText("/attach " + path)
				u.key(tcell.KeyEnter)
				u.wait(t, fmt.Sprintf("%d attachments", len(snapshots)))
			}
			const suffix = "\n\tλ final line  "
			original := "  original " + strings.Repeat("long text ", 24) + suffix
			pasteCancelInput(u, original)
			submitCancelInput(u, steer)
			u.wait(t, preview+"original")
			for _, path := range []string{textPath, dirPath, imagePath} {
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
			}
			// The current draft's attachments must be replaced, not merged.
			replacement := filepath.Join(u.runtime.Workspace.Root, "discarded-draft.txt")
			if err := os.WriteFile(replacement, []byte("discard this snapshot"), 0600); err != nil {
				t.Fatal(err)
			}
			u.typeText("/attach " + replacement)
			u.key(tcell.KeyEnter)
			u.wait(t, "1 attachments")
			u.typeText(command)
			u.key(tcell.KeyEnter)
			frame := u.wait(t, "Cancelled pending input")
			if !strings.Contains(frame, "λ final line") || !strings.Contains(frame, "3 attachments") {
				t.Fatal("composer did not restore complete input", frame)
			}
			// Resubmit without reattaching: removed paths cannot be read again.
			submitCancelInput(u, steer)
			u.wait(t, preview+"original")
			close(p.release)
			u.wait(t, "Restored settled.")
			u.wait(t, "Turn complete")
			var req provider.Request
			if !steer {
				olderReq := cancelTestRequest(t, p)
				if got := latestHumanInput(olderReq); got.Content != "older pending input" {
					t.Fatal("older queue was not preserved", got)
				}
			}
			req = cancelTestRequest(t, p)
			want := (contextbuild.Input{Text: original, Attachments: snapshots}).Message()
			got := latestHumanInput(req)
			want.InputSource = "queue"
			if steer {
				want.InputSource = "steer"
			}
			want.InputTimeMS = originalInputCommitTime(t, u, want.Content)
			if !reflect.DeepEqual(got, want) {
				b, _ := json.Marshal(got)
				t.Fatalf("restored input changed: %s\nwant %#v", b, want)
			}
			if steer {
				foundOlder := false
				for _, m := range req.Messages {
					foundOlder = foundOlder || m.Content == "older pending input"
				}
				if !foundOlder {
					t.Fatal("older steer was not preserved")
				}
			}
		})
	}
}

func originalInputCommitTime(t *testing.T, u *questionTestUI, content string) int64 {
	t.Helper()
	var count int
	var committed int64
	if err := u.runtime.Store.DB.QueryRow(`SELECT count(*),coalesce(min(created_ms),0) FROM entries
		WHERE kind='message' AND role='user' AND model_visible=1 AND source_id IS NULL
		AND json_extract(content_json,'$.content')=?`, content).Scan(&count, &committed); err != nil || count != 1 || committed <= 0 {
		t.Fatal("expected one original committed input", count, committed, err)
	}
	return committed
}

func latestHumanInput(req provider.Request) provider.Message {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" && !req.Messages[i].Runtime {
			return req.Messages[i]
		}
	}
	return provider.Message{}
}

func TestCancelPendingInputLIFOLeavesNoHistoryOrCheckpoints(t *testing.T) {
	for _, steer := range []bool{false, true} {
		name, command, preview := "queue", "/cancel-queue", "Queued · "
		if steer {
			name, command, preview = "steer", "/cancel-steer", "Steer · "
		}
		t.Run(name, func(t *testing.T) {
			u, p := newCancelInputUI(t, []provider.ScriptResponse{{Text: "Only initial settles."}})
			for _, text := range []string{"oldest pending", "newest pending"} {
				u.typeText(text)
				submitCancelInput(u, steer)
				u.wait(t, preview+text)
			}
			for _, text := range []string{"newest pending", "oldest pending"} {
				u.typeText(command)
				u.key(tcell.KeyEnter)
				u.wait(t, "> "+text)
				// Replace the restored draft with the next local command. It must
				// never become queued input merely because another command runs.
				u.key(tcell.KeyCtrlA)
				u.key(tcell.KeyCtrlK)
			}
			u.typeText(command)
			u.key(tcell.KeyEnter)
			needle := "no queued prompt to cancel"
			if steer {
				needle = "no pending steering instruction to cancel"
			}
			u.wait(t, needle)
			close(p.release)
			u.wait(t, "Turn complete")
			// An idle local command also must not start inference.
			u.typeText(command)
			u.key(tcell.KeyEnter)
			u.wait(t, needle)
			var requests, human, checkpoints int
			for query, result := range map[string]*int{
				"SELECT count(*) FROM model_requests":                                &requests,
				"SELECT count(*) FROM entries WHERE role='user' AND model_visible=1": &human,
				"SELECT count(*) FROM turns":                                         &checkpoints,
			} {
				if err := u.runtime.Store.DB.QueryRow(query).Scan(result); err != nil {
					t.Fatal(err)
				}
			}
			if count, _ := u.runtime.SteeringPreview(0); requests != 1 || human != 1 || checkpoints != 1 || count != 0 {
				t.Fatal("cancelled inputs were admitted or left pending", requests, human, checkpoints)
			}
		})
	}
}

func TestCancelCommandsDoNotSettleDismissedQuestions(t *testing.T) {
	u := newQuestionTestUI(t, &provider.Script{Responses: questionScript()})
	u.typeText("ask")
	u.key(tcell.KeyEnter)
	u.wait(t, "Choose a method?")
	u.key(tcell.KeyEscape)
	u.wait(t, "What would you like to do instead?")
	for _, command := range []string{"/cancel-queue", "/cancel-steer"} {
		u.typeText(command)
		u.key(tcell.KeyEnter)
		u.wait(t, "to cancel")
		form := u.runtime.PendingQuestion()
		if form == nil || !form.Dismissed {
			t.Fatal("local cancellation settled the dismissed question", form)
		}
	}
	var requests int
	if err := u.runtime.Store.DB.QueryRow("SELECT count(*) FROM model_requests").Scan(&requests); err != nil || requests != 1 {
		t.Fatal("cancellation resumed the model", requests, err)
	}
}

func TestCancelRestoredInputDoesNotSettleDismissedQuestions(t *testing.T) {
	for _, steer := range []bool{false, true} {
		command, preview := "/cancel-queue", "Queued · "
		if steer {
			command, preview = "/cancel-steer", "Steer · "
		}
		t.Run(command, func(t *testing.T) {
			u, p := newCancelInputUI(t, questionScript())
			u.typeText("restore rather than submit")
			submitCancelInput(u, steer)
			u.wait(t, preview+"restore rather than submit")
			close(p.release)
			u.wait(t, "Choose a method?")
			u.key(tcell.KeyEscape)
			u.wait(t, "What would you like to do instead?")
			u.typeText(command)
			u.key(tcell.KeyEnter)
			u.wait(t, "> restore rather than submit")
			form := u.runtime.PendingQuestion()
			if form == nil || !form.Dismissed {
				t.Fatal("restoring pending input settled the question", form)
			}
			var requests, human, checkpoints int
			for query, result := range map[string]*int{
				"SELECT count(*) FROM model_requests":                                &requests,
				"SELECT count(*) FROM entries WHERE role='user' AND model_visible=1": &human,
				"SELECT count(*) FROM turns":                                         &checkpoints,
			} {
				if err := u.runtime.Store.DB.QueryRow(query).Scan(result); err != nil {
					t.Fatal(err)
				}
			}
			if requests != 1 || human != 1 || checkpoints != 1 {
				t.Fatal("restoration admitted input", requests, human, checkpoints)
			}
		})
	}
}
