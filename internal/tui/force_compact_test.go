package tui

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	contextbuild "ttc/internal/context"
	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/session"
)

// Blank runtime replay needs names and presentation rows; other reads stay unused.
type compactControllerHistory struct{ historyReader }

func (compactControllerHistory) ReadReplay(context.Context, string, int64) (history.Replay, error) {
	return history.Replay{}, nil
}

func (compactControllerHistory) SubagentNames(context.Context, string) (map[string]string, error) {
	return nil, nil
}

func compactTestController() *frontendController {
	return &frontendController{
		frontend: &Frontend{Runtime: &session.Runtime{}},
		ctx:      context.Background(), history: compactControllerHistory{},
		view: newTranscript(), sidebar: newSidebar(),
		draft: newComposer("preserved draft"), notificationPaused: true,
		queue:       []contextbuild.Input{{Text: "queued input", Source: "queue"}},
		attachments: []contextbuild.Attachment{{Path: "draft.txt"}},
	}
}

func TestCompactionCommandsUseGenericBusyGuard(t *testing.T) {
	for _, command := range []string{"/compact", "/force-compact topic"} {
		for _, commandBusy := range []bool{false, true} {
			p := &helpTestProvider{requests: make(chan llm.Request, 4), release: make(chan struct{})}
			c := newAsyncTestController(t, p, func(f *Frontend) {
				if commandBusy {
					f.Runtime.Provider = &llm.Script{Responses: []llm.ScriptResponse{{Text: "Seed answer"}}}
					if _, err := f.Runtime.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Seed history"}); err != nil {
						t.Fatal(err)
					}
					f.Runtime.Provider = p
				}
			})
			if commandBusy {
				c.submitCommand("/compact")
			} else {
				c.submit(frontendSubmission{ready: true, text: "active task"})
			}
			waitControllerState(t, c, func(state session.CoordinatorState) bool { return state.Busy && state.CommandBusy == commandBusy })
			paused := c.notificationPaused
			c.submitCommand(command)
			result := waitControllerResult(t, c, session.RequestCommand)
			if result.Err == nil || !strings.Contains(result.Err.Error(), "idle turn") || result.Foreground || !c.busy || c.commandBusy != commandBusy || c.notificationPaused != paused {
				t.Fatal("busy compaction changed scheduling or bypassed admission", command, commandBusy, result)
			}
		}
	}
}

func TestCompactionCommandSchedulingMarksMarkdownWithoutResumingEarly(t *testing.T) {
	for _, command := range []string{"/compact", "/compact topic", "/force-compact", "/force-compact topic"} {
		t.Run(command, func(t *testing.T) {
			c := newAsyncTestController(t, nil, nil)
			// A failed initial attempt establishes coordinator-owned recovery pause.
			c.submitCommand("/compact")
			waitControllerResult(t, c, session.RequestCommand)
			if c.busy || !c.notificationPaused {
				t.Fatal("terminal result preceded its matching idle/paused projection")
			}
			c.submitCommand(command)
			waitControllerState(t, c, func(state session.CoordinatorState) bool { return state.Busy && state.CommandBusy })
			if !c.notificationPaused || c.started.IsZero() {
				t.Fatal("starting asynchronous recovery resumed notifications")
			}
			result := waitControllerResult(t, c, session.RequestCommand)
			if result.Command != strings.Fields(command)[0] || !result.Markdown || result.Err == nil || !strings.Contains(result.Err.Error(), "session is empty") {
				t.Fatalf("blank-session compaction result lost routing or Markdown: %+v", result)
			}
			if c.busy || !c.notificationPaused {
				t.Fatal("failed recovery result preceded its matching idle/paused projection")
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
				if err := c.handleUpdate(session.Update{
					State:  &session.CoordinatorState{NotificationPaused: failure != nil, Queue: queue},
					Result: &session.OperationResult{Foreground: true, Kind: session.RequestCommand, Command: command, Text: "# Context summary", Markdown: true, Err: failure},
				}); err != nil {
					t.Fatal(err)
				}
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
