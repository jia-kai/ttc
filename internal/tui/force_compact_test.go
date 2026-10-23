package tui

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/messagestream"
	"ttc/internal/session"
)

// Blank runtime replay needs names and stream rows; other reads stay unused.
type compactControllerHistory struct{ historyReader }

func (compactControllerHistory) StreamItems(context.Context, string) ([]messagestream.Item, error) {
	return nil, nil
}

func (compactControllerHistory) SubagentNames(context.Context, string) (map[string]string, error) {
	return nil, nil
}

func compactTestController() *frontendController {
	return &frontendController{
		frontend: &Frontend{Runtime: &session.Runtime{}},
		ctx:      context.Background(), history: compactControllerHistory{},
		view: newTranscript(), sidebar: newSidebar(), done: make(chan operationResult, 1),
		draft: newComposer("preserved draft"), notificationPaused: true,
		queue:       []contextbuild.Input{{Text: "queued input", Source: "queue"}},
		attachments: []contextbuild.Attachment{{Path: "draft.txt"}},
	}
}

func TestCompactionCommandsUseGenericBusyGuard(t *testing.T) {
	for _, command := range []string{"/compact", "/force-compact topic"} {
		for _, commandBusy := range []bool{false, true} {
			c := compactTestController()
			c.busy, c.commandBusy = true, commandBusy
			c.submitCommand(command)
			if !c.busy || c.commandBusy != commandBusy || !c.notificationPaused || len(c.done) != 0 {
				t.Fatal("busy compaction changed scheduling or notification state", command, commandBusy)
			}
			if len(c.view.lines) != 1 || !strings.Contains(c.view.lines[0].text, "Command requires an idle turn") {
				t.Fatal("compaction bypassed the generic command busy guard", command)
			}
		}
	}
}

func TestCompactionCommandSchedulingMarksMarkdownWithoutResumingEarly(t *testing.T) {
	for _, command := range []string{"/compact", "/compact topic", "/force-compact", "/force-compact topic"} {
		t.Run(command, func(t *testing.T) {
			c := compactTestController()
			c.submitCommand(command)
			if !c.busy || !c.commandBusy || !c.notificationPaused || c.started.IsZero() {
				t.Fatal("compaction did not use asynchronous command scheduling")
			}
			select {
			case result := <-c.done:
				if result.command != strings.Fields(command)[0] || !result.markdown || result.err == nil || !strings.Contains(result.err.Error(), "session is empty") {
					t.Fatalf("blank-session compaction result lost routing or Markdown: %+v", result)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("compaction command did not finish")
			}
			if !c.notificationPaused {
				t.Fatal("merely starting recovery resumed notifications")
			}
		})
	}
}

func TestCompactionCommandResultsPreservePendingInputAndResumeOnlyOnSuccess(t *testing.T) {
	for _, command := range []string{"/compact", "/force-compact"} {
		for _, failure := range []error{nil, errors.New("provider rejected request"), context.Canceled, context.DeadlineExceeded} {
			name := command + "/success"
			if failure != nil {
				name = command + "/" + failure.Error()
			}
			t.Run(name, func(t *testing.T) {
				c := compactTestController()
				c.busy, c.commandBusy = true, true
				// Failure must pause even when the user started from an unpaused state.
				c.notificationPaused = failure == nil
				queue := append([]contextbuild.Input(nil), c.queue...)
				attachments := append([]contextbuild.Attachment(nil), c.attachments...)
				c.handleOperationResult(operationResult{command: command, text: "# Context summary", markdown: true, err: failure})
				if c.busy || c.commandBusy || c.notificationPaused != (failure != nil) {
					t.Fatal("compaction result did not clear busy state or apply recovery policy")
				}
				if !reflect.DeepEqual(c.queue, queue) || !reflect.DeepEqual(c.attachments, attachments) || c.draft.text != "preserved draft" {
					t.Fatal("compaction result consumed queued input, attachments or draft")
				}
				if failure == nil {
					if c.modal.window == nil || !c.modal.window.Markdown || c.modal.window.Text != "# Context summary" || c.modal.window.Title != "Command result" {
						t.Fatal("successful compaction did not open the Markdown command popup", c.modal.window)
					}
				} else if c.modal.window != nil || len(c.view.lines) != 2 || !strings.Contains(c.view.lines[1].text, "pending notifications remain queued") {
					t.Fatal("failed compaction did not report queued-notification pause")
				}
			})
		}
	}
}

func TestForceCompactHelpExplainsNarrowExplicitAttempt(t *testing.T) {
	text, err := (&session.Runtime{}).Command("/help")
	if err != nil {
		t.Fatal(err)
	}
	var entry string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "- **/force-compact [FOCUS]**") {
			entry = line
		}
	}
	for _, required := range []string{"Explicitly attempt", "while idle", "only the summary input-size estimate guard", "provider may still reject", "retention, output limits and handoff checks remain unchanged"} {
		if !strings.Contains(entry, required) {
			t.Fatalf("force-compaction help omits %q: %s", required, entry)
		}
	}
}
