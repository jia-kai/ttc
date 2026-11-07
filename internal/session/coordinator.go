package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/render"
)

const coordinatorMailboxSize = 64
const coordinatorUpdateSize = 256
const coordinatorQueueSize = 128

// ErrCoordinatorFull means a bounded mailbox or input queue refused ownership.
var ErrCoordinatorFull = errors.New("session control mailbox is full; input was not accepted")

// RequestKind identifies an asynchronous human mutation or control operation.
type RequestKind string

const (
	RequestInput           RequestKind = "input"
	RequestCommand         RequestKind = "command"
	RequestCancelQueue     RequestKind = "cancel_queue"
	RequestCancelSteer     RequestKind = "cancel_steer"
	RequestInterrupt       RequestKind = "interrupt"
	RequestEOF             RequestKind = "eof"
	RequestModel           RequestKind = "model"
	RequestEasyModel       RequestKind = "easy_model"
	RequestBTW             RequestKind = "btw"
	RequestAnswer          RequestKind = "answer"
	RequestDismissQuestion RequestKind = "dismiss_question"
	RequestReopenQuestion  RequestKind = "reopen_question"
	RequestConfirmImage    RequestKind = "confirm_image"
	RequestBackground      RequestKind = "background"
	RequestReconnect       RequestKind = "reconnect"
	RequestLogin           RequestKind = "login"
)

// Request transfers immutable input/control arguments only when Submit succeeds.
// OperationID is the execution the human observed, not the current execution at
// processing time. Steer never targets a replacement execution.
type Request struct {
	Kind        RequestKind
	Generation  uint64
	OperationID uint64
	Input       contextbuild.Input
	Steer       bool
	Command     string
	Selection   *llm.Selection
	ID          string
	Answers     []Answer
	Point       *[2]int
	requestID   uint64
}

// ControlEffects freezes application-owned capabilities at coordinator startup.
// Callbacks run on the bounded control worker, never on the frontend loop.
type ControlEffects struct {
	Reconnect func() error
	Login     func(context.Context) ([]llm.ModelSpec, error)
}

// OperationResult is published after all synchronous updates from its operation.
// Restored returns original text and attachment snapshots when input was not used.
type OperationResult struct {
	RequestID   uint64
	OperationID uint64
	Generation  uint64
	Kind        RequestKind
	Foreground  bool
	Completion  *TurnCompletion
	Command     string
	Text        string
	Markdown    bool
	Err         error
	Restored    *contextbuild.Input
	Models      []llm.ModelSpec
}

// CoordinatorState is a read-only presentation snapshot. Queue contains only
// previews, never attachment payloads; drafts and modals belong to the frontend.
type CoordinatorState struct {
	Generation         uint64
	OperationID        uint64
	Busy               bool
	CommandBusy        bool
	NotificationPaused bool
	Queue              []contextbuild.Input
	EOF                bool
}

// Update is one ordered runtime publication or operation result. State updates
// share the same stream, so frontend projection needs no completion drain fence.
type Update struct {
	Event   *Event
	Result  *OperationResult
	State   *CoordinatorState
	Drained bool // EOF and every accepted request/result precede this terminal frontend fence.
}

type coordinatorSettlement struct {
	execution Execution
	result    OperationResult
}

type pendingHumanInput struct {
	input      contextbuild.Input
	futureOnly bool // Stale steering is a FIFO turn, never guidance to a replacement awaiting execution.
}

// SessionCoordinator owns human admission, FIFO scheduling and foreground
// lifecycle. Its mailbox and publication stream are bounded. Producers reserve
// publication capacity outside shared locks; critical commits use reserved slots.
// Shutdown disables delivery before joining producers, even if the UI has stopped.
type SessionCoordinator struct {
	runtime         *Runtime
	requests        chan Request
	updates         chan Update
	publications    chan Update
	capacity        chan struct{}
	reserveMu       sync.Mutex
	dispatcherDone  chan struct{}
	wakeup          chan struct{}
	settlements     chan coordinatorSettlement
	effects         chan Request
	effectResults   chan OperationResult
	effectsDone     chan struct{}
	interrupts      chan Request
	interruptsDone  chan struct{}
	effectCount     int
	pendingCommand  *Request
	controlEffects  ControlEffects
	effectCtx       context.Context
	cancelEffects   context.CancelFunc
	stop            chan struct{}
	done            chan struct{}
	startOnce       sync.Once
	closeOnce       sync.Once
	finalizeOnce    sync.Once
	shutdownOnce    sync.Once
	started         atomic.Bool
	nextRequest     atomic.Uint64
	requestsPending atomic.Int64
	publishMu       sync.Mutex
	state           atomic.Pointer[CoordinatorState]
	executions      executionRegistry
	childMu         sync.Mutex              // Owns child context admission and assignment lifecycle transitions.
	children        map[string]*codingChild // Retained contexts, distinct from transient execution identities.
	mainAccepting   bool                    // orderMu; terminal decision and human steering share this admission gate.
	// The fields below have one owner: loop.
	generation      uint64
	queue           []pendingHumanInput
	active          *Execution
	pending         *InputAdmission
	command         bool
	changingSession bool
	paused          bool
	eof             bool
	drained         bool
	workerDone      chan struct{}
}

func newSessionCoordinator(r *Runtime) *SessionCoordinator {
	c := &SessionCoordinator{runtime: r, requests: make(chan Request, coordinatorMailboxSize), updates: make(chan Update), publications: make(chan Update, coordinatorUpdateSize), capacity: make(chan struct{}, coordinatorUpdateSize), dispatcherDone: make(chan struct{}), wakeup: make(chan struct{}, 1), settlements: make(chan coordinatorSettlement, 1), stop: make(chan struct{}), done: make(chan struct{}), effects: make(chan Request, coordinatorMailboxSize), effectResults: make(chan OperationResult, coordinatorMailboxSize), effectsDone: make(chan struct{}), interrupts: make(chan Request, coordinatorMailboxSize), interruptsDone: make(chan struct{})}
	c.generation = r.Generation()
	c.effectCtx, c.cancelEffects = context.WithCancel(r.ctx)
	c.state.Store(&CoordinatorState{Generation: c.generation})
	return c
}

// Coordinator returns the session's single admission/lifecycle owner.
func (r *Runtime) Coordinator() *SessionCoordinator { return r.coordinator }

// Start enables ordered frontend publication and automatic main scheduling.
// A runtime may also be used synchronously without starting a frontend.
func (c *SessionCoordinator) Start(effects ControlEffects) {
	c.startOnce.Do(func() {
		c.controlEffects = effects
		c.generation = c.runtime.Generation()
		c.state.Store(&CoordinatorState{Generation: c.generation})
		c.started.Store(true)
		go c.dispatch()
		go c.controlLoop()
		go c.interruptLoop()
		go c.loop()
	})
}

// Updates is consumed by exactly one frontend for the coordinator lifetime.
func (c *SessionCoordinator) Updates() <-chan Update { return c.updates }

// Snapshot returns an immutable state copy; modifying it cannot affect admission.
func (c *SessionCoordinator) Snapshot() CoordinatorState {
	s := *c.state.Load()
	s.Queue = append([]contextbuild.Input(nil), s.Queue...)
	return s
}

// Submit is nonblocking. On error the caller retains input and its snapshots.
// On success the result (including rejected stale input) arrives on Updates.
func (c *SessionCoordinator) Submit(r Request) (uint64, error) {
	if !c.started.Load() {
		return 0, errors.New("session coordinator is not started")
	}
	select {
	case <-c.stop:
		return 0, errors.New("session coordinator is closed")
	default:
	}
	if r.Kind == RequestInput && c.state.Load().EOF {
		return 0, errors.New("session input has ended; input was not accepted")
	}
	r.requestID = c.nextRequest.Add(1)
	c.requestsPending.Add(1)
	if r.Kind == RequestInterrupt {
		select {
		case c.interrupts <- r:
			return r.requestID, nil
		default:
			c.requestsPending.Add(-1)
			return 0, ErrCoordinatorFull
		}
	}
	select {
	case c.requests <- r:
		return r.requestID, nil
	default:
		c.requestsPending.Add(-1)
		return 0, ErrCoordinatorFull
	}
}

func (c *SessionCoordinator) publish(u Update) {
	p := c.reserve(1)
	defer p.release()
	p.publish(u)
}

// publicationReservation guarantees nonblocking enqueue while a routing lock is
// held. Reservations are acquired before entering that lock and unused slots
// must be released on every error path.
type publicationReservation struct {
	coordinator  *SessionCoordinator
	remaining    int
	observations []Event
}

func (c *SessionCoordinator) reserve(n int) *publicationReservation {
	if n < 0 || n > coordinatorUpdateSize {
		panic("invalid session publication reservation")
	}
	p := &publicationReservation{coordinator: c}
	if !c.started.Load() {
		return p
	}
	// Serializing multi-slot acquisition avoids partial-reservation deadlocks.
	// This mutex is never acquired by registry/state getters or reserved commit.
	c.reserveMu.Lock()
	defer c.reserveMu.Unlock()
	for i := 0; i < n; i++ {
		select {
		case c.capacity <- struct{}{}:
			p.remaining++
		case <-c.stop:
			p.release()
			return p
		}
	}
	return p
}
func (p *publicationReservation) release() {
	for p.remaining > 0 {
		<-p.coordinator.capacity
		p.remaining--
	}
	p.flushObserver()
}
func (p *publicationReservation) flushObserver() {
	if p.coordinator.runtime.Emit != nil {
		for _, e := range p.observations {
			p.coordinator.runtime.Emit(e)
		}
	}
	p.observations = nil
}
func (p *publicationReservation) publish(u Update) {
	if p.remaining == 0 {
		return
	}
	c := p.coordinator
	c.publishMu.Lock()
	select {
	case <-c.stop:
		c.publishMu.Unlock()
		return
	default:
	}
	// Every queued/in-flight publication owns a token. Dispatcher releases it
	// only after frontend delivery, so this send cannot wait for channel space.
	c.publications <- u
	p.remaining--
	c.publishMu.Unlock()
}
func (c *SessionCoordinator) dispatch() {
	defer close(c.dispatcherDone)
	for {
		select {
		case <-c.stop:
			return
		case update := <-c.publications:
			if update.Event != nil && c.runtime.Emit != nil {
				c.runtime.Emit(*update.Event)
			}
			select {
			case c.updates <- update:
				<-c.capacity
			case <-c.stop:
				return
			}
		}
	}
}

func (r *Runtime) reservePublications(n int) *publicationReservation { return r.coordinator.reserve(n) }
func (r *Runtime) emitReserved(p *publicationReservation, e Event) {
	r.mu.Lock()
	if e.SessionID == "" {
		e.SessionID = r.current
	}
	e.Generation = r.generation
	r.mu.Unlock()
	e.OperationID = r.coordinator.executions.actorID(e.Actor)
	p.publish(Update{Event: &e})
	r.coordinator.wake()
	if !r.coordinator.started.Load() && r.Emit != nil {
		p.observations = append(p.observations, e)
	}
}
func (c *SessionCoordinator) wake() {
	if c == nil {
		return
	}
	select {
	case c.wakeup <- struct{}{}:
	default:
	}
}
func (c *SessionCoordinator) project() {
	s := &CoordinatorState{Generation: c.generation, Busy: c.active != nil, CommandBusy: c.command, NotificationPaused: c.paused, EOF: c.eof}
	if c.active != nil {
		s.OperationID = c.active.ID
	}
	for _, input := range c.queue {
		s.Queue = append(s.Queue, contextbuild.Input{Text: input.input.Text, Source: input.input.Source})
	}
	c.state.Store(s)
	c.publish(Update{State: s})
}
func (c *SessionCoordinator) result(r OperationResult) { c.publish(Update{Result: &r}) }

func (c *SessionCoordinator) loop() {
	defer close(c.done)
	c.project()
	for {
		c.refreshGeneration()
		c.schedule()
		select {
		case <-c.stop:
			c.join()
			return
		case <-c.runtime.ctx.Done():
			c.closeOnce.Do(func() { close(c.stop) })
			c.join()
			return
		case r := <-c.requests:
			c.handle(r)
			c.requestsPending.Add(-1)
		case settled := <-c.settlements:
			c.settle(settled)
		case result := <-c.effectResults:
			c.settleControl(result)
		case <-c.wakeup:
			c.resumeAwaiting()
		}
	}
}
func (c *SessionCoordinator) refreshGeneration() {
	if g := c.runtime.Generation(); g != c.generation {
		for _, input := range c.queue {
			restored := input.input
			c.result(OperationResult{Kind: RequestInput, Generation: c.generation, Restored: &restored, Err: errors.New("session changed before queued input admission; input was restored, not submitted")})
		}
		clear(c.queue)
		c.generation = g
		c.queue = nil
		c.pending = nil
		c.paused = false
		c.project()
	}
}
func (c *SessionCoordinator) schedule() {
	if c.active != nil {
		return
	}
	if c.effectCount != 0 {
		return
	}
	if c.pendingCommand != nil {
		r := *c.pendingCommand
		c.pendingCommand = nil
		c.start(nil, r)
		return
	}
	if e, err := c.runtime.ApplyModel(""); err != nil {
		c.paused = true
		c.result(OperationResult{Kind: RequestModel, Generation: c.generation, Err: err})
		c.project()
	} else if e.Kind != "" {
		c.paused = false
		c.runtime.emit(e)
		c.project()
	}
	if len(c.queue) > 0 {
		input := c.queue[0].input
		c.queue[0] = pendingHumanInput{}
		c.queue = c.queue[1:]
		c.start(c.runtime.PrepareInput(input), Request{})
		return
	}
	if c.requestsPending.Load() != 0 {
		return
	}
	if !c.paused && c.runtime.HasNotifications() {
		c.start(nil, Request{})
		return
	}
	if c.eof && !c.drained {
		c.drained = true
		c.publish(Update{Drained: true})
	}
}
func (c *SessionCoordinator) start(p *InputAdmission, r Request) {
	execution := c.executions.begin("main", "main", "", c.generation)
	var initial *contextbuild.Input
	if p != nil {
		p.mu.Lock()
		input := p.input
		initial = &input
		p.mu.Unlock()
	}
	c.active = &execution
	c.pending = p
	c.command = r.Kind == RequestCommand
	if c.command {
		switch strings.Fields(r.Command)[0] {
		case "/new", "/clear", "/load", "/undo", "/redo", "/branch":
			c.changingSession = true
		}
	}
	c.workerDone = make(chan struct{})
	c.project()
	go func() {
		defer close(c.workerDone)
		result := OperationResult{RequestID: r.requestID, OperationID: execution.ID, Generation: execution.Generation, Kind: RequestInput, Foreground: true}
		if r.Kind == RequestCommand {
			result.Kind = RequestCommand
			result.Command = strings.Fields(r.Command)[0]
			result.Text, result.Err = c.runtime.Command(r.Command)
			result.Markdown = result.Command == "/compact" || result.Command == "/force-compact"
			if json.Valid([]byte(result.Text)) {
				result.Text = render.Fence(result.Text, "json")
				result.Markdown = true
			}
		} else if p == nil {
			result.Completion, result.Err = c.runtime.Run(nil)
		} else {
			result.Completion, result.Err = c.runtime.RunInput(p)
		}
		if result.Completion == nil && result.Err != nil {
			result.Restored = initial
		}
		c.executions.setPhase(execution.ID, "settling")
		c.settlements <- coordinatorSettlement{execution: execution, result: result}
	}()
}
func (c *SessionCoordinator) settle(s coordinatorSettlement) {
	c.executions.finish(s.execution.ID)
	c.active = nil
	c.pending = nil
	c.command = false
	c.changingSession = false
	c.workerDone = nil
	if s.result.Err != nil && (s.result.Kind == RequestInput || s.result.Command == "/compact" || s.result.Command == "/force-compact") {
		c.paused = true
	}
	if s.result.Err == nil {
		switch s.result.Command {
		case "/compact", "/force-compact", "/load", "/new":
			c.paused = false
		}
	}
	// Publish settlement before scheduling any replacement execution.
	c.refreshGeneration()
	c.project()
	c.result(s.result)
}
func (c *SessionCoordinator) join() {
	c.cancelForShutdown()
	if c.workerDone != nil {
		<-c.workerDone
	}
}

// Close disables frontend delivery, cancels work and privately joins its owners.
// It releases publication backpressure without requiring a frontend drain.
func (c *SessionCoordinator) Close() {
	c.closeOnce.Do(func() { close(c.stop) })
	c.cancelEffects()
	c.cancelForShutdown()
	if c.started.Load() {
		<-c.done
		<-c.dispatcherDone
		<-c.effectsDone
		<-c.interruptsDone
	}
	c.finalizeOnce.Do(func() {
		// No admission effect can launch work after this sweep. Assignment cleanup
		// joins its owned work before its lifecycle entry is retired.
		if c.runtime.Jobs != nil {
			c.runtime.Jobs.Close()
		}
		c.runtime.stopNaming()
		c.executions.mu.Lock()
		clear(c.executions.active)
		clear(c.executions.actors)
		clear(c.executions.jobs)
		clear(c.executions.interrupted)
		c.executions.mu.Unlock()
		c.childMu.Lock()
		clear(c.children)
		c.childMu.Unlock()
		c.queue, c.pending, c.active, c.pendingCommand = nil, nil, nil, nil
		c.state.Store(&CoordinatorState{Generation: c.runtime.Generation()})
	})
}

func (c *SessionCoordinator) cancelForShutdown() {
	c.shutdownOnce.Do(func() {
		if c.started.Load() {
			<-c.interruptsDone
		}
		// An accepted /quit interruption must win over cleanup cancellation,
		// even when teardown starts before the cancellation worker is scheduled.
		for {
			select {
			case request := <-c.interrupts:
				c.interruptObserved(request)
			default:
				c.runtime.CancelForeground()
				return
			}
		}
	})
}

func (c *SessionCoordinator) handle(r Request) {
	result := OperationResult{RequestID: r.requestID, Kind: r.Kind, OperationID: r.OperationID, Generation: r.Generation}
	if r.Generation != c.generation {
		result.Err = errors.New("session changed before input/control admission; restored input was not submitted")
		if r.Kind == RequestInput {
			input := r.Input
			result.Restored = &input
		}
		c.result(result)
		return
	}
	switch r.Kind {
	case RequestInput:
		if c.eof {
			input := r.Input
			result.Restored = &input
			result.Err = errors.New("session input ended before admission; input was restored")
			break
		}
		c.acceptInput(r, &result)
		if result.Err == nil {
			c.paused = false
		}
	case RequestCommand:
		if c.active != nil || c.pendingCommand != nil || r.OperationID != 0 {
			result.Err = errors.New("command requires the observed idle turn; interrupt work first")
		} else if strings.TrimSpace(r.Command) == "" {
			result.Err = errors.New("empty command")
		} else if c.effectCount > 0 {
			c.pendingCommand = &r
			c.project()
			return
		} else {
			c.start(nil, r)
			return
		}
	case RequestEOF:
		c.eof = true
	case RequestAnswer, RequestDismissQuestion, RequestReopenQuestion:
		if c.pendingCommand != nil || c.changingSession {
			result.Err = errors.New("wait for the current history command")
			break
		}
		// These bounded, in-memory mutations must commit in mailbox order with
		// input admission. A slow effect must not let a following prompt overtake
		// its question dismissal and leave the suspended tool waiting forever.
		switch r.Kind {
		case RequestAnswer:
			result.Err = c.runtime.AnswerQuestion(r.ID, r.Answers)
		case RequestDismissQuestion:
			result.Err = c.runtime.DismissQuestion(r.ID)
		case RequestReopenQuestion:
			result.Err = c.runtime.ReopenQuestion(r.ID)
		}
	case RequestCancelQueue:
		var input contextbuild.Input
		if len(c.queue) > 0 {
			last := len(c.queue) - 1
			input = c.queue[last].input
			c.queue[last] = pendingHumanInput{}
			c.queue = c.queue[:last]
		} else if c.pending != nil {
			input, result.Err = c.pending.Cancel()
		} else {
			result.Err = errors.New("no queued prompt to cancel")
		}
		if result.Err == nil {
			result.Restored = &input
			result.Text = "Cancelled pending input · restored to composer"
		}
	default:
		c.dispatchControl(r)
		return
	}
	c.result(result)
	c.project()
}
func (c *SessionCoordinator) acceptInput(r Request, result *OperationResult) {
	restore := func(err error) { input := r.Input; result.Restored = &input; result.Err = err }
	if err := validateSteeringInput(r.Input); err != nil {
		restore(err)
		return
	}
	if err := c.runtime.checkContext(false); err != nil {
		restore(err)
		return
	}
	if c.active != nil && r.OperationID == c.active.ID {
		if redirected, err := c.runtime.RedirectDismissedQuestion(r.Input); err != nil {
			restore(err)
			return
		} else if redirected {
			return
		}
		if c.runtime.Awaiting("main") {
			inputs := append(c.awaitingInputs(), r.Input)
			if err := c.runtime.SteerInputs(inputs); err != nil {
				if !errors.Is(err, errMainNotSteerable) {
					restore(err)
					return
				}
			} else {
				c.consumeAwaitingInputs()
				return
			}
		}
		if r.Steer && !c.command {
			if err := c.runtime.Steer(r.Input); err == nil {
				return
			} else if !errors.Is(err, errMainNotSteerable) {
				restore(err)
				return
			} else if c.executions.phase(c.active.ID) == "preparing" {
				restore(errors.New("observed turn is still preparing; input was restored, not steered"))
				return
			}
			// The request may have completed before its private join reached the loop.
			// Preserve the input as FIFO work; never redirect it into a replacement.
			result.Text = "Observed turn completed; steering was queued as a new input."
		}
	} else if r.Steer {
		result.Text = "Observed turn completed; steering was queued as a new input."
	}
	if len(c.queue) >= coordinatorQueueSize {
		restore(ErrCoordinatorFull)
		return
	}
	input := r.Input
	input.Source = "normal"
	if c.active != nil || len(c.queue) > 0 {
		input.Source = "queue"
	}
	c.queue = append(c.queue, pendingHumanInput{input: input, futureOnly: r.Steer})
	if result.Text == "" && c.active != nil {
		result.Text = fmt.Sprintf("Queued input · %d unsent", len(c.queue))
	}
}
func (c *SessionCoordinator) resumeAwaiting() {
	if c.active == nil || c.command || len(c.queue) == 0 || !c.runtime.Awaiting("main") {
		return
	}
	inputs := c.awaitingInputs()
	if len(inputs) == 0 {
		return
	}
	if err := c.runtime.SteerInputs(inputs); err != nil {
		return
	}
	c.consumeAwaitingInputs()
	c.project()
}

func (c *SessionCoordinator) awaitingInputs() []contextbuild.Input {
	var inputs []contextbuild.Input
	for _, pending := range c.queue {
		if !pending.futureOnly {
			inputs = append(inputs, pending.input)
		}
	}
	return inputs
}

func (c *SessionCoordinator) consumeAwaitingInputs() {
	kept := c.queue[:0]
	for _, pending := range c.queue {
		if pending.futureOnly {
			kept = append(kept, pending)
		}
	}
	clear(c.queue[len(kept):])
	c.queue = kept
}
