package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/history"
	"ttc/internal/jobs"
	"ttc/internal/llm"
	"ttc/internal/session"
	"ttc/internal/skills"
	"ttc/internal/tool"
	"ttc/internal/workspace"
)

type completionWriter struct {
	bytes.Buffer
	onWrite func(string)
}

func (w *completionWriter) Write(p []byte) (int, error) {
	if w.onWrite != nil {
		w.onWrite(string(p))
	}
	return w.Buffer.Write(p)
}

func TestTurnCompletionOrdersUpdatesBeforeIdlePresentation(t *testing.T) {
	for _, plain := range []bool{false, true} {
		t.Run(fmt.Sprintf("plain=%v", plain), func(t *testing.T) {
			output := &completionWriter{}
			p := &questionTestProvider{Script: llm.Script{Responses: []llm.ScriptResponse{{Text: "Last turn output"}}}, ready: make(chan struct{}), release: make(chan struct{})}
			c := newAsyncTestController(t, p, func(f *Frontend) { f.Output = output })
			c.frontend.Plain = plain
			output.Reset()
			output.onWrite = func(text string) {
				if strings.Contains(text, "Turn complete") && c.busy {
					t.Error("plain completion published before idle transition")
				}
			}
			c.submit(frontendSubmission{ready: true, text: "Earlier turn input"})
			waitControllerState(t, c, func(state session.CoordinatorState) bool { return state.Busy })
			select {
			case <-p.ready:
			case <-time.After(3 * time.Second):
				t.Fatal("provider did not start")
			}
			c.frontend.Runtime.Publish(session.Event{Kind: "status", Text: "Before settlement sentinel"})
			for {
				update := nextControllerUpdate(t, c)
				if update.Result != nil && update.Result.Foreground {
					t.Fatal("operation completed before provider/tool settlement")
				}
				if update.Event != nil && update.Event.Text == "Before settlement sentinel" {
					break
				}
			}
			if !c.busy {
				t.Fatal("stream update marked operation idle")
			}
			close(p.release)
			var end *session.TurnCompletion
			for end == nil {
				update := nextControllerUpdate(t, c)
				if update.Result != nil && update.Result.Foreground {
					if update.Result.Err != nil {
						t.Fatal(update.Result.Err)
					}
					end = update.Result.Completion
				}
			}
			if c.busy {
				waitControllerState(t, c, func(state session.CoordinatorState) bool { return !state.Busy })
			}
			var text strings.Builder
			for _, row := range c.view.lines {
				fmt.Fprintln(&text, row.text)
			}
			if strings.Count(text.String(), end.Text) != 1 || strings.Index(text.String(), "Last turn output") > strings.Index(text.String(), end.Text) {
				t.Fatal("ordered stream lost, duplicated or reordered terminal output", text.String())
			}
			if plain && (strings.Count(output.String(), end.Text) != 1 || strings.Index(output.String(), "Last turn output") > strings.Index(output.String(), end.Text)) {
				t.Fatal("plain output reordered completion", output.String())
			}
			// Background publication after this operation's terminal result is not
			// drained into its completion and cannot clear a later busy execution.
			c.frontend.Runtime.Provider = &helpTestProvider{requests: make(chan llm.Request, 4), release: make(chan struct{})}
			c.submit(frontendSubmission{ready: true, text: "Next operation"})
			waitControllerState(t, c, func(state session.CoordinatorState) bool { return state.Busy })
			c.frontend.Runtime.Publish(session.Event{Kind: "status", Text: "Next operation background output"})
			for {
				update := nextControllerUpdate(t, c)
				if update.Event != nil && update.Event.Text == "Next operation background output" {
					break
				}
			}
			if !c.busy {
				t.Fatal("previous completion affected the next operation")
			}
		})
	}
}

func TestTurnCompletionOrderedBackgroundTrafficAndEventError(t *testing.T) {
	t.Run("background after result", func(t *testing.T) {
		output := &completionWriter{}
		c := newAsyncTestController(t, &llm.Script{Responses: []llm.ScriptResponse{{Text: "Earlier output"}}}, func(f *Frontend) { f.Output = output })
		output.Reset()
		output.onWrite = func(text string) {
			if strings.Contains(text, "Turn complete") {
				c.frontend.Runtime.Publish(session.Event{Kind: "status", Text: "Concurrent background update"})
			}
		}
		c.submit(frontendSubmission{ready: true, text: "task"})
		seenCompletion, seenBackground := false, false
		for !seenBackground {
			update := nextControllerUpdate(t, c)
			if update.Result != nil && update.Result.Foreground {
				seenCompletion = true
			}
			if update.Event != nil && update.Event.Text == "Concurrent background update" {
				if !seenCompletion {
					t.Fatal("background was folded into an earlier completion")
				}
				seenBackground = true
			}
		}
		if strings.Index(output.String(), "Turn complete") > strings.Index(output.String(), "Concurrent background update") {
			t.Fatal("background reordered terminal output", output.String())
		}
	})
	t.Run("invalid ordered event joins active worker", func(t *testing.T) {
		p := &helpTestProvider{requests: make(chan llm.Request, 4), release: make(chan struct{})}
		c := newAsyncTestController(t, p, nil)
		c.submit(frontendSubmission{ready: true, text: "active task"})
		waitControllerState(t, c, func(state session.CoordinatorState) bool { return state.Busy })
		c.frontend.Runtime.Publish(session.Event{Kind: "stream_item"})
		for {
			select {
			case update := <-c.updates:
				if err := c.handleUpdate(update); err != nil {
					if !strings.Contains(err.Error(), "saved item identity") {
						t.Fatal(err)
					}
					joined := make(chan struct{})
					go func() { c.coordinator.Close(); close(joined) }()
					select {
					case <-joined:
					case <-time.After(3 * time.Second):
						t.Fatal("event failure left shutdown waiting for the active worker")
					}
					return
				}
			case <-time.After(3 * time.Second):
				t.Fatal("invalid ordered event did not reach frontend")
			}
		}
	})
}

func TestTurnCompletionHandlesUnadmittedTurns(t *testing.T) {
	for _, failure := range []error{nil, errors.New("admission failed"), context.Canceled} {
		c := &frontendController{frontend: &Frontend{Runtime: &session.Runtime{}, Output: io.Discard}, view: newTranscript(), busy: true}
		if err := c.handleUpdate(session.Update{State: &session.CoordinatorState{NotificationPaused: failure != nil}, Result: &session.OperationResult{Foreground: true, Err: failure}}); err != nil {
			t.Fatal(err)
		}
		if c.busy || c.notificationPaused != (failure != nil) {
			t.Fatalf("unadmitted result lost lifecycle: failure=%v busy=%v paused=%v", failure, c.busy, c.notificationPaused)
		}
		for _, row := range c.view.lines {
			if strings.Contains(row.text, "Turn complete") {
				t.Fatal("unadmitted turn acquired completion row")
			}
		}
	}
}

// Gate resumed coding independently from summarization so all ordered updates
// can be collected without presenting them until durable history has advanced.
type completionCompactionProvider struct {
	*inputCompactionProvider
	coding                     int
	handoffReady, finishCoding chan struct{}
}

func (p *completionCompactionProvider) Stream(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
	if !req.NoTools {
		p.coding++
		if p.coding == 3 {
			close(p.handoffReady)
			select {
			case <-p.finishCoding:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return p.inputCompactionProvider.Stream(ctx, req, emit)
}

func TestTurnCompletionAutomaticContinuationUsesFrozenReplayCut(t *testing.T) {
	for _, plain := range []bool{false, true} {
		for _, prefixFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("plain=%v/prefixFirst=%v", plain, prefixFirst), func(t *testing.T) {
				store, err := history.Open(filepath.Join(t.TempDir(), "data"))
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				w, err := workspace.Open(t.TempDir(), store)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				catalog, err := skills.Discover(ctx, w.Root, "")
				if err != nil {
					t.Fatal(err)
				}
				p := &completionCompactionProvider{inputCompactionProvider: &inputCompactionProvider{summarizing: make(chan struct{}), finishSummary: make(chan struct{}), continued: make(chan struct{}), finishContinuation: make(chan struct{})}, handoffReady: make(chan struct{}), finishCoding: make(chan struct{})}
				close(p.finishSummary)
				close(p.finishContinuation)
				r, err := session.New(ctx, store, w, p, llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}, "", catalog, tool.WebSearchConfig{}, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				r.AutoName = false
				for _, input := range []string{"Earlier large task", "Recently completed task"} {
					if _, err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: input}); err != nil {
						t.Fatal(err)
					}
				}
				if err := r.RequestModel(p.selection); err != nil {
					t.Fatal(err)
				}
				output := &completionWriter{}
				c := &frontendController{frontend: &Frontend{Runtime: r, Plain: true, Input: strings.NewReader(""), Output: output}}
				if err := c.setup(ctx); err != nil {
					t.Fatal(err)
				}
				defer c.close(nil)
				c.frontend.Plain = plain
				c.view = newTranscript()
				output.Reset()
				output.onWrite = func(text string) {
					if strings.Contains(text, "Turn complete") && c.busy {
						t.Error("completion printed before idle settlement")
					}
				}
				before := r.Current()
				if _, err := r.Jobs.StartTask("main", jobs.TaskOptions{Kind: "shell", Label: "notification turn", Background: true}, func(context.Context, io.Writer, io.Writer) (jobs.TaskResult, error) { return jobs.TaskResult{}, nil }); err != nil {
					t.Fatal(err)
				}
				select {
				case <-p.handoffReady:
				case <-time.After(3 * time.Second):
					t.Fatal("coding did not resume after automatic compaction")
				}
				if r.Current() == before {
					t.Fatal("fixture did not perform automatic compaction")
				}
				const steer = "Unique post-handoff steer"
				// The turn remains gated, so steering targets this exact execution.
				state := r.Coordinator().Snapshot()
				if _, err := r.Coordinator().Submit(session.Request{Kind: session.RequestInput, Generation: state.Generation, OperationID: state.OperationID, Steer: true, Input: contextbuild.Input{Text: steer}}); err != nil {
					t.Fatal(err)
				}
				var buffered []session.Update
				var continuation session.Event
				// Wait for explicit steering acknowledgement before releasing coding.
				for {
					select {
					case update := <-c.updates:
						buffered = append(buffered, update)
						if update.Event != nil && update.Event.Kind == "continuation" {
							continuation = *update.Event
						}
						if update.Result != nil && update.Result.Kind == session.RequestInput && !update.Result.Foreground {
							if update.Result.Err != nil {
								t.Fatal(update.Result.Err)
							}
							goto admitted
						}
					case <-time.After(3 * time.Second):
						t.Fatal("steering acknowledgement missing")
					}
				}
			admitted:
				if prefixFirst {
					for _, update := range buffered {
						if err := c.handleUpdate(update); err != nil {
							t.Fatal(err)
						}
						if update.Event != nil && update.Event.Kind == "continuation" && !c.busy {
							t.Fatal("continuation marked worker idle")
						}
					}
					buffered = nil
				}
				close(p.finishCoding)
				var end *session.TurnCompletion
				for end == nil {
					select {
					case update := <-c.updates:
						buffered = append(buffered, update)
						if update.Result != nil && update.Result.Foreground {
							if update.Result.Err != nil {
								t.Fatal(update.Result.Err)
							}
							end = update.Result.Completion
						}
					case <-time.After(3 * time.Second):
						t.Fatal("continuation turn did not settle")
					}
				}
				if continuation.Continuation == nil || continuation.SessionID != r.Current() || continuation.Continuation.EntryTip <= 0 || continuation.Continuation.EntryTip >= end.EntryID {
					t.Fatalf("handoff did not freeze pre-completion cut: %+v", continuation)
				}
				snapshot := &frontendController{frontend: &Frontend{Runtime: r, Output: io.Discard}, ctx: ctx, history: store, view: newTranscript(), sidebar: newSidebar()}
				for range 2 {
					snapshot.replay(continuation.Continuation.EntryTip)
				}
				priorCompletions := 0
				for _, row := range snapshot.view.lines {
					if strings.HasPrefix(row.text, "Turn complete") {
						priorCompletions++
					}
					if row.id > continuation.Continuation.EntryTip || strings.Contains(row.text, steer) || strings.Contains(row.text, "Input compaction response 2.") {
						t.Fatalf("post-cut publication leaked into handoff replay: %+v", row)
					}
				}
				if priorCompletions == 0 {
					t.Fatal("handoff lost earlier retained completion")
				}
				// Both incremental and delayed rendering consume one immutable order;
				// there is no result-first branch and no frontend snapshot drain.
				for _, update := range buffered {
					if err := c.handleUpdate(update); err != nil {
						t.Fatal(err)
					}
					if prefixFirst && update.Event != nil && update.Event.Kind == "continuation" && !c.busy {
						t.Fatal("continuation marked worker idle")
					}
				}
				if c.busy {
					waitControllerState(t, c, func(state session.CoordinatorState) bool { return !state.Busy })
				}
				var transcript strings.Builder
				for _, row := range c.view.lines {
					fmt.Fprintln(&transcript, row.text)
				}
				for _, text := range []string{steer, "Input compaction response 2.", "Input compaction response 3."} {
					if strings.Count(transcript.String(), text) != 1 || plain && strings.Count(output.String(), text) != 1 {
						t.Fatalf("lost or duplicated %q: transcript=%q plain=%q", text, transcript.String(), output.String())
					}
				}
				if strings.Count(transcript.String(), "Turn complete") != priorCompletions+1 || plain && strings.Count(output.String(), "Turn complete") != 1 {
					t.Fatalf("completion lost or replayed early: transcript=%q plain=%q", transcript.String(), output.String())
				}
			})
		}
	}
}
