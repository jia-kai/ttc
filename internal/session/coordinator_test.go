package session

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/historytest"
	"ttc/internal/jobs"
	"ttc/internal/llm"
)

func startCoordinator(t *testing.T, r *Runtime, effects ControlEffects) *SessionCoordinator {
	t.Helper()
	r.Emit = nil
	c := r.Coordinator()
	c.Start(effects)
	t.Cleanup(c.Close)
	return c
}
func coordinatorSubmit(t *testing.T, c *SessionCoordinator, r Request) uint64 {
	t.Helper()
	if r.Generation == 0 {
		r.Generation = c.runtime.Generation()
	}
	id, err := c.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func coordinatorUpdate(t *testing.T, c *SessionCoordinator, match func(Update) bool) Update {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case u := <-c.Updates():
			if match(u) {
				return u
			}
		case <-timer.C:
			t.Fatal("coordinator update timed out")
			return Update{}
		}
	}
}
func coordinatorResult(t *testing.T, c *SessionCoordinator, id uint64) *OperationResult {
	t.Helper()
	return coordinatorUpdate(t, c, func(u Update) bool { return u.Result != nil && u.Result.RequestID == id }).Result
}
func coordinatorStarted(t *testing.T, c *SessionCoordinator) uint64 {
	t.Helper()
	return coordinatorUpdate(t, c, func(u Update) bool { return u.State != nil && u.State.Busy }).State.OperationID
}
func coordinatorFinished(t *testing.T, c *SessionCoordinator, id uint64) *OperationResult {
	t.Helper()
	return coordinatorUpdate(t, c, func(u Update) bool { return u.Result != nil && u.Result.Foreground && u.Result.OperationID == id }).Result
}

func TestCoordinatorSlowConsumerBackpressureOutsideRouting(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	c := startCoordinator(t, r, ControlEffects{})
	coordinatorUpdate(t, c, func(u Update) bool { return u.State != nil })
	filled, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		for i := 0; i < coordinatorUpdateSize; i++ {
			r.Publish(Event{Kind: "status", Text: "buffered"})
		}
		close(filled)
		r.Publish(Event{Kind: "status", Text: "last"})
	}()
	select {
	case <-filled:
	case <-time.After(3 * time.Second):
		t.Fatal("bounded publisher did not fill")
	}
	// The next reservation is blocked by the absent reader, not by routing.
	if !r.routeMu.TryLock() {
		t.Fatal("slow UI held the continuation routing lock")
	}
	r.routeMu.Unlock()
	select {
	case <-finished:
		t.Fatal("publisher bypassed bounded backpressure")
	default:
	}
	count := 0
	for count < coordinatorUpdateSize+1 {
		u := coordinatorUpdate(t, c, func(u Update) bool { return u.Event != nil })
		count++
		if count == coordinatorUpdateSize+1 && u.Event.Text != "last" {
			t.Fatal("publication reordered", u.Event)
		}
	}
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("publisher did not resume")
	}
	if r.ctx.Err() != nil {
		t.Fatal("healthy slow UI cancelled the session", r.ctx.Err())
	}
}

func TestCoordinatorBurstEventsPrecedeForegroundSettlement(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	const chunks = 1500
	r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		for i := 0; i < chunks; i++ {
			if err := emit(llm.StreamEvent{Kind: "text", Text: "x"}); err != nil {
				return err
			}
		}
		return emit(llm.StreamEvent{Kind: "completed"})
	}}
	c := startCoordinator(t, r, ControlEffects{})
	coordinatorSubmit(t, c, Request{Kind: RequestInput, Input: contextbuild.Input{Text: "burst"}})
	seen := 0
	terminal := false
	coordinatorUpdate(t, c, func(u Update) bool {
		if u.Event != nil && u.Event.Kind == "delta" {
			if terminal {
				t.Error("delta followed terminal result")
			}
			seen++
		}
		if u.Result != nil && u.Result.Foreground {
			terminal = true
			if u.Result.Err != nil {
				t.Error(u.Result.Err)
			}
			return true
		}
		return false
	})
	if seen != chunks {
		t.Fatalf("received %d deltas before settlement, want %d", seen, chunks)
	}
}

func TestCoordinatorInterruptBeforeWorkerSetupAndStaleInterrupt(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	ready, release := make(chan struct{}), make(chan struct{})
	r.Provider = &childProvider{stream: func(ctx context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		close(ready)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := emit(llm.StreamEvent{Kind: "text", Text: "B complete"}); err != nil {
			return err
		}
		return emit(llm.StreamEvent{Kind: "completed"})
	}}
	c := startCoordinator(t, r, ControlEffects{})
	r.runMu.Lock()
	locked := true
	defer func() {
		if locked {
			r.runMu.Unlock()
		}
	}()
	coordinatorSubmit(t, c, Request{Kind: RequestInput, Input: contextbuild.Input{Text: "A must not admit"}})
	a := coordinatorStarted(t, c)
	r.mu.Lock()
	notInstalled := r.activeCancel == nil
	r.mu.Unlock()
	if !notInstalled {
		t.Fatal("test did not gate worker setup")
	}
	cancelID := coordinatorSubmit(t, c, Request{Kind: RequestInterrupt, OperationID: a})
	coordinatorResult(t, c, cancelID)
	r.runMu.Unlock()
	locked = false
	result := coordinatorFinished(t, c, a)
	if !errors.Is(result.Err, context.Canceled) || result.Completion != nil {
		t.Fatal("early interrupt was lost", result)
	}
	assertInputHistoryCounts(t, r, 0)
	coordinatorSubmit(t, c, Request{Kind: RequestInput, Input: contextbuild.Input{Text: "B must survive stale cancellation"}})
	b := coordinatorStarted(t, c)
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("replacement did not start")
	}
	stale := coordinatorSubmit(t, c, Request{Kind: RequestInterrupt, OperationID: a})
	coordinatorResult(t, c, stale)
	close(release)
	result = coordinatorFinished(t, c, b)
	if result.Err != nil || result.Completion == nil {
		t.Fatal("stale cancellation targeted replacement", result)
	}
}

func TestCoordinatorBlockedAsideAdmissionDoesNotBlockInterrupt(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "seed")
	ready := make(chan struct{})
	mainCancelled := make(chan struct{})
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		if strings.Contains(req.ConversationID, "/btw_") {
			return emit(llm.StreamEvent{Kind: "completed"})
		}
		close(ready)
		<-ctx.Done()
		close(mainCancelled)
		return ctx.Err()
	}}
	c := startCoordinator(t, r, ControlEffects{})
	coordinatorSubmit(t, c, Request{Kind: RequestInput, Input: contextbuild.Input{Text: "active operation"}})
	operation := coordinatorStarted(t, c)
	<-ready
	r.routeMu.Lock()
	locked := true
	defer func() {
		if locked {
			r.routeMu.Unlock()
		}
	}()
	aside := coordinatorSubmit(t, c, Request{Kind: RequestBTW, OperationID: operation, Command: "blocked aside"})
	// Effect-count projection confirms dispatch before issuing the interrupt.
	coordinatorUpdate(t, c, func(u Update) bool { return u.State != nil && u.State.Busy })
	interrupt := coordinatorSubmit(t, c, Request{Kind: RequestInterrupt, OperationID: operation})
	coordinatorResult(t, c, interrupt)
	select {
	case <-mainCancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("blocked admission prevented provider cancellation")
	}
	r.routeMu.Unlock()
	locked = false
	var asideDone, mainDone bool
	coordinatorUpdate(t, c, func(u Update) bool {
		if result := u.Result; result != nil {
			asideDone = asideDone || result.RequestID == aside
			if result.Foreground && result.OperationID == operation {
				mainDone = true
				if !errors.Is(result.Err, context.Canceled) {
					t.Error("blocked aside prevented interruption", result)
				}
			}
		}
		return asideDone && mainDone
	})
}

func TestCoordinatorStaleSteerDoesNotResumeReplacementAwait(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	releaseA := make(chan struct{})
	var requests atomic.Int32
	stale := contextbuild.Input{Text: "stale A input", Attachments: []contextbuild.Attachment{{Path: "removed.txt", Kind: "text", Text: "immutable attachment"}}}
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		n := requests.Add(1)
		switch n {
		case 1:
			select {
			case <-releaseA:
			case <-ctx.Done():
				return ctx.Err()
			}
		case 2:
			call := llm.ToolCall{ID: "await-B", Name: "await", Arguments: []byte(`{}`)}
			if err := emit(llm.StreamEvent{Kind: "call", Call: &call}); err != nil {
				return err
			}
		case 3:
			for _, m := range req.Messages {
				if strings.Contains(m.Content, stale.Text) {
					t.Error("stale steering entered replacement's awaiting request")
				}
			}
		case 4:
			found := false
			for _, m := range req.Messages {
				if strings.Contains(m.Content, stale.Text) && strings.Contains(m.Content, "immutable attachment") {
					found = true
					if m.InputSource == "steer" {
						t.Error("stale input retained steering semantics")
					}
				}
			}
			if !found {
				t.Error("new FIFO turn lost input or immutable snapshot")
			}
		default:
			t.Errorf("unexpected request %d", n)
		}
		if err := emit(llm.StreamEvent{Kind: "text", Text: "done"}); err != nil {
			return err
		}
		return emit(llm.StreamEvent{Kind: "completed"})
	}}
	c := startCoordinator(t, r, ControlEffects{})
	coordinatorSubmit(t, c, Request{Kind: RequestInput, Input: contextbuild.Input{Text: "A"}})
	a := coordinatorStarted(t, c)
	queued := coordinatorSubmit(t, c, Request{Kind: RequestInput, OperationID: a, Input: contextbuild.Input{Text: "B"}})
	coordinatorResult(t, c, queued)
	close(releaseA)
	b := uint64(0)
	coordinatorUpdate(t, c, func(u Update) bool {
		if u.State != nil && u.State.Busy && u.State.OperationID != a {
			b = u.State.OperationID
		}
		return u.Event != nil && u.Event.Kind == "awaiting"
	})
	if b == 0 {
		t.Fatal("replacement identity was not published before awaiting")
	}
	request := coordinatorSubmit(t, c, Request{Kind: RequestInput, OperationID: a, Steer: true, Input: stale})
	result := coordinatorResult(t, c, request)
	if result.Err != nil || !strings.Contains(result.Text, "queued as a new input") {
		t.Fatal("stale steering was not reclassified", result)
	}
	coordinatorUpdate(t, c, func(u Update) bool { return u.State != nil && len(u.State.Queue) == 1 })
	if count, _ := r.SteeringPreview(0); count != 0 {
		t.Fatal("stale input steered replacement", count)
	}
	if requests.Load() != 2 {
		t.Fatal("stale input woke replacement", requests.Load())
	}
	resume := coordinatorSubmit(t, c, Request{Kind: RequestInput, OperationID: b, Steer: true, Input: contextbuild.Input{Text: "resume B"}})
	coordinatorResult(t, c, resume)
	seen := 0
	coordinatorUpdate(t, c, func(u Update) bool {
		if u.Result != nil && u.Result.Foreground {
			seen++
		}
		return seen == 2
	})
	if requests.Load() != 4 {
		t.Fatal("future FIFO input did not run after replacement", requests.Load())
	}
}

func TestCoordinatorCrossGenerationQueuedInputReturnsSnapshots(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	c := startCoordinator(t, r, ControlEffects{})
	original := contextbuild.Input{Text: "return original", Attachments: []contextbuild.Attachment{{Path: "gone.txt", Kind: "text", Text: "frozen"}}}
	r.runMu.Lock()
	locked := true
	defer func() {
		if locked {
			r.runMu.Unlock()
		}
	}()
	command := coordinatorSubmit(t, c, Request{Kind: RequestCommand, Command: "/new"})
	coordinatorStarted(t, c)
	input := coordinatorSubmit(t, c, Request{Kind: RequestInput, Input: original})
	coordinatorResult(t, c, input)
	old := r.Generation()
	r.runMu.Unlock()
	locked = false
	var returned *OperationResult
	commandFinished := false
	coordinatorUpdate(t, c, func(u Update) bool {
		if u.Result != nil {
			commandFinished = commandFinished || u.Result.RequestID == command
			if u.Result.Restored != nil {
				returned = u.Result
			}
		}
		return commandFinished && returned != nil
	})
	expected := original
	expected.Source = "queue"
	if !reflect.DeepEqual(*returned.Restored, expected) || returned.Err == nil || r.Generation() == old {
		t.Fatal("reset silently dropped accepted input snapshots", returned, r.Generation())
	}
	staleID := coordinatorSubmit(t, c, Request{Kind: RequestInput, Generation: old, Input: original, Steer: true})
	staleResult := coordinatorResult(t, c, staleID)
	if staleResult.Restored == nil || !reflect.DeepEqual(*staleResult.Restored, original) {
		t.Fatal("cross-generation submission lost snapshots", staleResult)
	}
	var count int
	if err := historytest.DB(t, r.Store).QueryRow("SELECT count(*) FROM turns").Scan(&count); err != nil || count != 0 {
		t.Fatal("stale input was admitted", count, err)
	}
}

func TestCoordinatorCloseWithoutConsumerJoinsAllExecutionKinds(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "seed")
	for _, kind := range []string{"subagent", "btw"} {
		r.coordinator.executions.begin("main/"+kind, kind, "", r.Generation())
		_, err := r.Jobs.StartTask("main/"+kind, jobs.TaskOptions{Kind: kind, Label: kind, Background: true}, func(ctx context.Context, _ io.Writer, _ io.Writer) (jobs.TaskResult, error) {
			<-ctx.Done()
			return jobs.TaskResult{}, ctx.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	ready := make(chan struct{})
	r.Provider = &childProvider{stream: func(ctx context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		close(ready)
		for i := 0; i < 1000; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := emit(llm.StreamEvent{Kind: "text", Text: "x"}); err != nil {
				return err
			}
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	c := startCoordinator(t, r, ControlEffects{})
	coordinatorSubmit(t, c, Request{Kind: RequestInput, Input: contextbuild.Input{Text: "shutdown"}})
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("main did not start")
	}
	executions := c.Executions()
	if len(executions) != 3 {
		t.Fatal("execution lifecycle was not shared", executions)
	}
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown waited for absent UI")
	}
	if live := c.Executions(); len(live) != 0 {
		t.Fatal("shutdown left execution entries", live)
	}
	if state := c.Snapshot(); state.Busy || len(state.Queue) != 0 {
		t.Fatal("shutdown left active owner state", state)
	}
	if _, err := c.Submit(Request{Kind: RequestInput}); err == nil {
		t.Fatal("closed coordinator accepted input")
	}
}

func TestCoordinatorPreparingSteerRestoresInsteadOfClaimingCompletion(t *testing.T) {
	r, _ := runtimeFixture(t, []llm.ScriptResponse{{Text: "main reply"}})
	c := startCoordinator(t, r, ControlEffects{})
	r.runMu.Lock()
	locked := true
	defer func() {
		if locked {
			r.runMu.Unlock()
		}
	}()
	coordinatorSubmit(t, c, Request{Kind: RequestInput, Input: contextbuild.Input{Text: "preparing main"}})
	operation := coordinatorStarted(t, c)
	original := contextbuild.Input{Text: "preparing steer", Attachments: []contextbuild.Attachment{{Path: "snapshot.txt", Kind: "text", Text: "immutable"}}}
	id := coordinatorSubmit(t, c, Request{Kind: RequestInput, OperationID: operation, Steer: true, Input: original})
	result := coordinatorResult(t, c, id)
	if result.Err == nil || !strings.Contains(result.Err.Error(), "preparing") || result.Restored == nil || !reflect.DeepEqual(*result.Restored, original) {
		t.Fatal("preparing input was mislabeled completed or lost", result)
	}
	if state := c.Snapshot(); len(state.Queue) != 0 {
		t.Fatal("preparing steer silently became ordinary input", state)
	}
	r.runMu.Unlock()
	locked = false
	if result := coordinatorFinished(t, c, operation); result.Err != nil {
		t.Fatal(result.Err)
	}
}

func TestCoordinatorInvalidAndFatalSteeringReturnSnapshots(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	ready, release := make(chan struct{}), make(chan struct{})
	r.Provider = &childProvider{stream: func(ctx context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		close(ready)
		select {
		case <-release:
			return emit(llm.StreamEvent{Kind: "completed"})
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	c := startCoordinator(t, r, ControlEffects{})
	coordinatorSubmit(t, c, Request{Kind: RequestInput, Input: contextbuild.Input{Text: "main"}})
	operation := coordinatorStarted(t, c)
	<-ready
	for _, fatal := range []bool{false, true} {
		original := contextbuild.Input{Text: "invalid \xff", Attachments: []contextbuild.Attachment{{Path: "gone", Kind: "text", Text: "snapshot"}}}
		if fatal {
			original.Text = "valid but unusable"
			r.mu.Lock()
			r.fatalCompaction = "test failure"
			r.mu.Unlock()
		}
		id := coordinatorSubmit(t, c, Request{Kind: RequestInput, OperationID: operation, Steer: true, Input: original})
		result := coordinatorResult(t, c, id)
		if result.Err == nil || result.Restored == nil || !reflect.DeepEqual(*result.Restored, original) || strings.Contains(result.Text, "queued") {
			t.Fatal("invalid/fatal steering was silently rerouted", result)
		}
	}
	r.mu.Lock()
	r.fatalCompaction = ""
	r.mu.Unlock()
	close(release)
	if result := coordinatorFinished(t, c, operation); result.Err != nil {
		t.Fatal(result.Err)
	}
}

func TestCoordinatorMailboxRejectsWithoutTakingInputOwnership(t *testing.T) {
	c := &SessionCoordinator{requests: make(chan Request, coordinatorMailboxSize), stop: make(chan struct{})}
	c.state.Store(&CoordinatorState{})
	c.started.Store(true)
	original := contextbuild.Input{Text: "bounded", Attachments: []contextbuild.Attachment{{Path: "gone", Kind: "text", Text: "snapshot"}}}
	for i := 0; i < coordinatorMailboxSize; i++ {
		if _, err := c.Submit(Request{Kind: RequestInput, Input: original}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Submit(Request{Kind: RequestInput, Input: original}); !errors.Is(err, ErrCoordinatorFull) {
		t.Fatal("full mailbox did not reject promptly", err)
	}
	if original.Text != "bounded" || original.Attachments[0].Text != "snapshot" {
		t.Fatal("rejected input was mutated", original)
	}
}

func TestCoordinatorEOFDrainsTerminalAndControlResultsBeforeExit(t *testing.T) {
	r, _ := runtimeFixture(t, []llm.ScriptResponse{{Text: "final reply"}})
	c := startCoordinator(t, r, ControlEffects{})
	coordinatorSubmit(t, c, Request{Kind: RequestInput, Input: contextbuild.Input{Text: "one prompt"}})
	eof := coordinatorSubmit(t, c, Request{Kind: RequestEOF})
	terminal, eofResult, idle := false, false, false
	coordinatorUpdate(t, c, func(u Update) bool {
		if u.State != nil && !u.State.Busy {
			idle = true
		}
		if u.Result != nil {
			eofResult = eofResult || u.Result.RequestID == eof
			terminal = terminal || u.Result.Foreground
		}
		return u.Drained
	})
	if !terminal || !eofResult || !idle {
		t.Fatal("EOF overtook ordered terminal/control publications", terminal, eofResult, idle)
	}
	input := contextbuild.Input{Text: "too late", Attachments: []contextbuild.Attachment{{Path: "gone", Kind: "text", Text: "snapshot"}}}
	if _, err := c.Submit(Request{Kind: RequestInput, Generation: r.Generation(), Input: input}); err == nil {
		t.Fatal("EOF accepted new input without a consumer")
	}
}
