package tui

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/session"
)

func TestForceReconnectIsLocalBusyOrIdleAndPreservesPendingWork(t *testing.T) {
	for _, active := range []string{"idle", "model", "command"} {
		t.Run(active, func(t *testing.T) {
			var output bytes.Buffer
			calls := 0
			p := &helpTestProvider{requests: make(chan llm.Request, 4), release: make(chan struct{})}
			c := newAsyncTestController(t, p, func(f *Frontend) {
				f.Output = &output
				f.Reconnect = func() error { calls++; return nil }
				if active == "command" {
					f.Runtime.Provider = &llm.Script{Responses: []llm.ScriptResponse{{Text: "Seed answer"}}}
					if _, err := f.Runtime.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Seed history"}); err != nil {
						t.Fatal(err)
					}
					f.Runtime.Provider = p
				}
			})
			if active != "idle" {
				if active == "command" {
					c.submitCommand("/compact")
				} else {
					c.submit(frontendSubmission{ready: true, text: "active task"})
				}
				waitControllerState(t, c, func(state session.CoordinatorState) bool {
					return state.Busy && state.CommandBusy == (active == "command")
				})
				if _, err := c.coordinator.Submit(session.Request{Kind: session.RequestInput, Generation: c.observedGeneration, OperationID: c.observedOperationID, Input: contextbuild.Input{Text: "queued input", Attachments: []contextbuild.Attachment{{Path: "queued.txt", Text: "snapshot", Kind: "file"}}}}); err != nil {
					t.Fatal(err)
				}
				waitControllerResult(t, c, session.RequestInput)
				waitControllerState(t, c, func(state session.CoordinatorState) bool { return len(state.Queue) == 1 })
			}
			c.draft.set("preserved draft")
			c.attachments = []contextbuild.Attachment{{Path: "retained.txt"}}
			queue := append([]contextbuild.Input(nil), c.queue...)
			attachments := append([]contextbuild.Attachment(nil), c.attachments...)
			busy, commandBusy, operation := c.busy, c.commandBusy, c.operationID
			c.submitCommand("/force-reconnect")
			result := waitControllerResult(t, c, session.RequestReconnect)
			waitControllerState(t, c, func(state session.CoordinatorState) bool { return !state.NotificationPaused })
			if result.Err != nil || calls != 1 || c.busy != busy || c.commandBusy != commandBusy || c.operationID != operation || c.notificationPaused {
				t.Fatalf("reconnect changed operation ownership: result=%+v calls=%d busy=%v commandBusy=%v paused=%v", result, calls, c.busy, c.commandBusy, c.notificationPaused)
			}
			if c.draft.text != "preserved draft" || !reflect.DeepEqual(c.queue, queue) || !reflect.DeepEqual(c.attachments, attachments) {
				t.Fatal("reconnect discarded pending work")
			}
			if !strings.Contains(output.String(), "Provider connections reset") || !strings.Contains(output.String(), "not been verified") {
				t.Fatal("reconnect confirmation misleading or missing", output.String())
			}
		})
	}
}

func TestForceReconnectFailureUnsupportedAndUsage(t *testing.T) {
	for _, tc := range []struct {
		command string
		reset   func() error
		want    string
	}{
		{"/force-reconnect", nil, "does not support reconnecting"},
		{"/force-reconnect", func() error { return errors.New("reset failed") }, "reset failed"},
		{"/force-reconnect extra", func() error { t.Fatal("invalid command reset connections"); return nil }, "Usage: /force-reconnect"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			p := &helpTestProvider{requests: make(chan llm.Request, 4), release: make(chan struct{})}
			c := newAsyncTestController(t, p, func(f *Frontend) { f.Reconnect = tc.reset })
			c.submit(frontendSubmission{ready: true, text: "active task"})
			waitControllerState(t, c, func(state session.CoordinatorState) bool { return state.Busy })
			c.view = newTranscript()
			c.submitCommand(tc.command)
			if tc.reset != nil && tc.command == "/force-reconnect" {
				waitControllerResult(t, c, session.RequestReconnect)
			}
			if !c.busy || len(c.view.lines) != 1 || !strings.Contains(c.view.lines[0].text, tc.want) {
				t.Fatalf("failed reconnect changed state or hid diagnostic: busy=%v rows=%+v", c.busy, c.view.lines)
			}
		})
	}
}

func TestForceReconnectResumesPausedOnlyOnSuccess(t *testing.T) {
	for _, failure := range []error{nil, errors.New("reset failed")} {
		name := "success"
		if failure != nil {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			c := newAsyncTestController(t, nil, func(f *Frontend) { f.Reconnect = func() error { return failure } })
			c.submitCommand("/compact")
			waitControllerResult(t, c, session.RequestCommand)
			if c.busy || !c.notificationPaused {
				t.Fatal("failed command result preceded its idle/paused projection")
			}
			c.draft.set("retained while paused")
			c.attachments = []contextbuild.Attachment{{Path: "snapshot.txt", Kind: "file", Text: "immutable snapshot"}}
			c.submitCommand("/force-reconnect")
			result := waitControllerResult(t, c, session.RequestReconnect)
			if !errors.Is(result.Err, failure) {
				t.Fatal("reconnect lost its effect error", result.Err, failure)
			}
			waitControllerState(t, c, func(state session.CoordinatorState) bool { return state.NotificationPaused == (failure != nil) })
			if c.busy || c.commandBusy || c.draft.text != "retained while paused" || len(c.attachments) != 1 || c.attachments[0].Text != "immutable snapshot" {
				t.Fatal("reconnect changed foreground or pending-input ownership")
			}
		})
	}
}

func TestForceReconnectIsDiscoverable(t *testing.T) {
	found := false
	for _, item := range commandCompletions(completionQuery{prefix: "force-reconnect"}).items {
		found = found || item.value == "/force-reconnect"
	}
	if !found || !strings.Contains(newCommandMenu().Window.Text, "/force-reconnect") {
		t.Fatal("reconnect missing from suggestions or palette")
	}
	text, err := (&session.Runtime{}).Command("/help")
	if err != nil || !strings.Contains(text, "/force-reconnect") || !strings.Contains(text, "already failed turns are not replayed") {
		t.Fatal("help omits reconnect semantics", err)
	}
}
