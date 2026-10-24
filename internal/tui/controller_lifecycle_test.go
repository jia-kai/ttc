package tui

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v3"
)

type controllerLifecycleScreen struct {
	tcell.Screen
	initErr, resumeErr     error
	initialized, finalized int
	onResume               func()
}

func (s *controllerLifecycleScreen) Init() error {
	s.initialized++
	return s.initErr
}

func (s *controllerLifecycleScreen) Fini() { s.finalized++ }

func (s *controllerLifecycleScreen) Resume() error {
	if s.onResume != nil {
		s.onResume()
	}
	return s.resumeErr
}

type controllerPromptHistory struct {
	historyReader
	err error
}

func (h controllerPromptHistory) PromptHistory(context.Context) ([]string, error) {
	return nil, h.err
}

func TestControllerTerminalInputError(t *testing.T) {
	for _, editing := range []bool{false, true} {
		c := &frontendController{editing: editing}
		_, err := c.receiveScreenEvent(tcell.NewEventError(io.ErrUnexpectedEOF), true)
		if !errors.Is(err, io.ErrUnexpectedEOF) || !strings.Contains(err.Error(), "terminal input") {
			t.Fatalf("terminal error while editing=%v: %v", editing, err)
		}
	}
}

func TestControllerPartialSetupCancelsOnlyAcquiredResources(t *testing.T) {
	for _, stage := range []string{"prompt history", "terminal init"} {
		t.Run(stage, func(t *testing.T) {
			failure := errors.New(stage)
			screen := &controllerLifecycleScreen{}
			reader := controllerPromptHistory{}
			if stage == "prompt history" {
				reader.err = failure
			} else {
				screen.initErr = failure
			}
			c := &frontendController{frontend: &Frontend{Screen: screen}, history: reader}
			err := c.setup(context.Background())
			if !errors.Is(err, failure) {
				t.Fatalf("setup error = %v, want %v", err, failure)
			}
			if got := c.close(err); got != failure {
				t.Fatalf("cleanup changed original error: %v", got)
			}
			if !errors.Is(c.ctx.Err(), context.Canceled) {
				t.Fatal("partial setup did not cancel its context")
			}
			wantInit := 0
			if stage == "terminal init" {
				wantInit = 1
			}
			if screen.initialized != wantInit || screen.finalized != 0 {
				t.Fatalf("Init/Fini = %d/%d, want %d/0", screen.initialized, screen.finalized, wantInit)
			}
		})
	}
}

func TestControllerCleanupCancelsJoinsAndResumesBeforeDependencies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	initial, resume := errors.New("run failure"), errors.New("resume failure")
	var order []string
	c := &frontendController{
		ctx: ctx, cancelRun: cancel,
		editing: true, editorSuspended: true, attaching: true,
		editorDone: make(chan operationResult, 1), attachmentDone: make(chan attachmentResult, 1),
	}
	screen := &controllerLifecycleScreen{resumeErr: resume}
	c.screen = screen
	// Workers send only after cancellation. The attachment deliberately waits
	// for resume, proving the shutdown path retains the original join order.
	resumed := make(chan struct{})
	screen.onResume = func() {
		if ctx.Err() == nil || len(c.editorDone) != 0 {
			t.Error("screen resumed before cancellation/editor join")
		}
		order = append(order, "resume")
		close(resumed)
	}
	go func() { <-ctx.Done(); c.editorDone <- operationResult{} }()
	go func() { <-ctx.Done(); <-resumed; c.attachmentDone <- attachmentResult{} }()
	c.onClose(func() { order = append(order, "screen") })
	c.onClose(func() {
		if len(c.attachmentDone) != 0 {
			t.Error("dependencies closed before attachment join")
		}
		order = append(order, "renderer")
	})
	c.onClose(c.stopInteractions)
	got := c.close(initial)
	if !errors.Is(got, initial) || !errors.Is(got, resume) {
		t.Fatalf("cleanup error = %v, want original and resume failure", got)
	}
	if want := []string{"resume", "renderer", "screen"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("cleanup order = %v, want %v", order, want)
	}
}

func TestControllerPlainInputRetainsLinesAndEOF(t *testing.T) {
	c := &frontendController{frontend: &Frontend{Input: strings.NewReader("first\n\nlast\n"), Plain: true}}
	c.setupInput()
	defer c.close(nil)
	for _, want := range []string{"first", "", "last", "/eof"} {
		select {
		case got := <-c.inputs:
			if got.text != want {
				t.Fatalf("plain input = %q, want %q", got.text, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("plain input stalled before %q", want)
		}
	}
}

func TestControllerCleanupStillUnwindsAfterPanic(t *testing.T) {
	c := &frontendController{}
	var order []string
	c.onClose(func() { order = append(order, "first acquired") })
	c.onClose(func() { panic("cleanup failure") })
	c.onClose(func() { order = append(order, "last acquired") })
	func() {
		defer func() {
			if got := recover(); got != "cleanup failure" {
				t.Errorf("cleanup panic = %v", got)
			}
		}()
		c.close(nil)
	}()
	if want := []string{"last acquired", "first acquired"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("panic cleanup order = %v, want %v", order, want)
	}
}

func TestControllerComposerSubmissionSnapshotsSteeringChoice(t *testing.T) {
	for _, tc := range []struct {
		name          string
		busy, command bool
		modifiers     tcell.ModMask
		steering      bool
	}{
		{name: "idle"},
		{name: "steer", busy: true, steering: true},
		{name: "queue", busy: true, modifiers: tcell.ModAlt},
		{name: "history command", busy: true, command: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &frontendController{draft: newComposer("  preserved input  "), view: newTranscript(), busy: tc.busy, commandBusy: tc.command}
			got := c.handleComposerKey(tcell.NewEventKey(tcell.KeyEnter, "", tc.modifiers))
			if !got.ready || got.text != "  preserved input  " || got.steering != tc.steering {
				t.Fatalf("submission snapshot = %+v", got)
			}
			c.draft.set("next draft")
			if got.text != "  preserved input  " || !c.view.followTail {
				t.Fatal("later composer state changed the submission snapshot")
			}
		})
	}
}
