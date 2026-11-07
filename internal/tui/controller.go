package tui

import (
	"bufio"
	"context"
	"errors"
	"time"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/catalog"
	contextbuild "ttc/internal/context"
	"ttc/internal/graphics"
	"ttc/internal/session"
	"ttc/internal/workspace"
)

// frontendController owns mutable frontend state on Run's event loop. Workers
// receive snapshots and return results through channels; they never update it.
type frontendController struct {
	frontend  *Frontend
	history   historyReader
	ctx       context.Context
	cancelRun context.CancelFunc
	cleanups  []func() // Registered as resources are acquired; executed in reverse order.
	runErr    error    // Accumulates terminal resume failure during cleanup.

	screen             tcell.Screen
	tty                *terminalTTY
	g                  *graphics.Kitty
	graphicsWarning    string
	graphicsActivation <-chan struct{}
	workspaceUpdates   <-chan workspace.GitInfo
	renderer           *imageRenderer
	renderResults      <-chan renderReply
	ticker             *time.Ticker
	inputs             chan input
	keys               <-chan tcell.Event
	modelUpdates       <-chan catalog.Update

	view        *transcript
	sidebar     *sidebar
	modal       modalState
	fullscreen  bool
	copyView    *transcript // Frozen transcript while terminal selection is enabled.
	copyFocused int
	focused     int
	dirty       bool
	syncFrame   bool // Explicit source replay refreshes the terminal, not only its previous cell diff.
	names       map[string]string
	streamRows  map[int64]int // Grouped request header IDs to stable transcript positions.

	coordinator         *session.SessionCoordinator
	updates             <-chan session.Update
	operationID         uint64
	observedOperationID uint64
	observedGeneration  uint64
	observedCommandBusy bool
	plainGateRequest    uint64 // Plain command sequencing, not runtime admission ownership.
	busy                bool
	commandBusy         bool
	// Read-only coordinator projection; never used to admit or schedule input.
	notificationPaused bool
	queue              []contextbuild.Input
	queueGeneration    uint64
	started            time.Time
	activity           turnActivity
	ended              bool // Read-only coordinator drained fence; the frontend never schedules EOF.

	draft           composer
	recall          promptHistory
	attachments     []contextbuild.Attachment
	attachmentEpoch uint64 // Advances when the draft attachment collection is replaced, not appended.
	pendingRestores map[uint64]restoreBaseline
	completer       *pathCompleter
	completion      *completionMenu
	lastQuery       completionQuery
	hadQuery        bool
	esc             bool
	viewChord       bool
	paste           pasteBatch
	batchingEvents  bool // Paste edits flush once per bounded receive batch.

	editorDone        chan operationResult
	editing           bool
	editorSuspended   bool
	applyEditorResult func(operationResult) // Captures the edited draft/question and tab.
	attachmentDone    chan attachmentResult
	attaching         bool
	cancelAttachment  context.CancelFunc

	question              *questionDialog // Open inline form, independent of foreground modal windows.
	retainedQuestion      *questionDialog // Keeps dismissed answers and tab state available for reopening.
	deferredQuestionID    string
	questionReopenRequest uint64 // Only this acknowledgment may still open the inline form.
	composerQuestionID    string // Composer acceptance suppresses this form's queued publication until explicit reopening.
	questionRow           int    // Only the singleton live status row needs reconciliation; -1 means absent.
	pendingBTW            []session.Event
}

type frontendSubmission struct {
	text            string
	ready, steering bool
	generation      uint64
	operationID     uint64
}

type attachmentResult struct {
	attachment contextbuild.Attachment
	generation uint64
	err        error
}

func (c *frontendController) onClose(cleanup func()) {
	c.cleanups = append(c.cleanups, cleanup)
}

func (c *frontendController) close(err error) (runErr error) {
	c.runErr = err
	defer func() {
		runErr = c.runErr
		c.cleanups = nil
	}()
	// Use real defers so every acquired resource is unwound even if another
	// cleanup panics, just as when these defers lived directly in Run.
	for _, cleanup := range c.cleanups {
		defer cleanup()
	}
	return err
}

func (c *frontendController) setup(ctx context.Context) error {
	c.ctx, c.cancelRun = context.WithCancel(ctx)
	c.onClose(c.cancelRun)
	if c.history == nil {
		c.history = c.frontend.Runtime.Store
	}
	if !c.frontend.Plain {
		prompts, err := c.history.PromptHistory(c.ctx)
		if err != nil {
			return err
		}
		for _, prompt := range prompts {
			c.recall.add(prompt)
		}
	}
	if err := c.setupTerminal(); err != nil {
		return err
	}
	c.setupInput()
	c.coordinator = c.frontend.Runtime.Coordinator()
	c.updates = c.coordinator.Updates()
	c.coordinator.Start(session.ControlEffects{Reconnect: c.frontend.Reconnect, Login: c.frontend.Login})
	c.onClose(c.stopForeground)
	c.queueGeneration = c.frontend.Runtime.Generation()
	c.observedGeneration = c.queueGeneration
	c.view = newTranscript()
	c.sidebar = newSidebar()
	c.copyFocused = -1
	c.dirty = true
	c.editorDone = make(chan operationResult, 1)
	if c.screen != nil {
		c.sidebar.workspace = workspace.GitInfo{Cwd: c.frontend.Runtime.Workspace.Root}
		monitor := newWorkspaceMonitor(c.ctx, c.frontend.Runtime.Workspace.Root)
		c.onClose(monitor.close)
		c.workspaceUpdates = monitor.updates
	}
	if err := c.setupRenderer(); err != nil {
		return err
	}
	c.setupComposer()
	c.names = map[string]string{}
	c.questionRow = -1
	c.add("TTC · Linux terminal agent · /help", 0)
	c.replay(0)
	for _, notice := range c.frontend.Notices {
		c.add(notice, 0)
	}
	c.ticker = time.NewTicker(250 * time.Millisecond)
	c.onClose(c.ticker.Stop)
	return nil
}

func (c *frontendController) setupTerminal() error {
	if c.frontend.Plain {
		return nil
	}
	v, err := openTerminalView(c.ctx, c.frontend.Screen, c.frontend.Graphics)
	if err != nil {
		return err
	}
	c.screen, c.tty, c.g = v.screen, v.tty, v.graphics
	c.graphicsWarning, c.graphicsActivation = v.warning, v.activation
	c.onClose(v.close)
	return nil
}

func (c *frontendController) setupInput() {
	c.inputs = make(chan input, 32)
	c.modelUpdates = c.frontend.ModelUpdates
	// Stop delivery on exit without trying to close an externally owned reader.
	stop := make(chan struct{})
	c.onClose(func() { close(stop) })
	if c.screen != nil {
		c.keys = c.screen.EventQ()
		return
	}
	reader, inputs := c.frontend.Input, c.inputs
	go func() {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 4096), 8<<20)
		for scanner.Scan() {
			select {
			case inputs <- input{scanner.Text()}:
			case <-stop:
				return
			}
		}
		select {
		case inputs <- input{"/eof"}:
		case <-stop:
		}
	}()
}

func (c *frontendController) setupRenderer() error {
	if c.g != nil {
		var err error
		c.renderer, err = newImageRenderer(c.ctx, c.g, c.frontend.Runtime.Workspace.Root)
		if err != nil {
			return err
		}
		c.onClose(func() { _ = c.renderer.close() })
		c.renderer.pendingClick = c.frontend.Runtime.ImageClickPending
		c.view.layout = c.renderer.layout
		c.renderResults = c.renderer.results
	}
	c.frontend.Runtime.EnableImageClicks(c.g != nil)
	c.onClose(func() { c.frontend.Runtime.EnableImageClicks(false) })
	return nil
}

func (c *frontendController) setupComposer() {
	c.draft = newComposer("")
	c.completer = newPathCompleter(c.ctx)
	c.onClose(c.completer.close)
	c.completion = &completionMenu{}
	c.attachmentDone = make(chan attachmentResult, 1)
	c.onClose(c.stopInteractions)
}

// stopInteractions cancels and joins workers before closing their dependencies.
// A suspended screen must resume before the eventual Fini, even after an error.
func (c *frontendController) stopInteractions() {
	c.modal.clear()
	c.cancelRun()
	if c.editing {
		<-c.editorDone
	}
	if c.editorSuspended {
		c.runErr = errors.Join(c.runErr, c.screen.Resume())
	}
	if c.attaching {
		<-c.attachmentDone
	}
}

func (c *frontendController) stopForeground() {
	c.coordinator.Close()
}
