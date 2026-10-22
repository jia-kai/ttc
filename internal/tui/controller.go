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
	names       map[string]string
	seenPrompts map[string]int64

	done        chan operationResult
	busy        bool
	commandBusy bool
	// Failures pause automatic notification retry until explicit input or recovery.
	notificationPaused bool
	queue              []contextbuild.Input
	pendingInput       *session.InputAdmission
	queueGeneration    uint64
	started            time.Time
	activity           turnActivity
	eof                bool

	draft          composer
	recall         promptHistory
	attachments    []contextbuild.Attachment
	completer      *pathCompleter
	completion     *completionMenu
	lastQuery      completionQuery
	hadQuery       bool
	esc            bool
	viewChord      bool
	paste          pasteBatch
	batchingEvents bool // Paste edits flush once per bounded receive batch.

	editorDone        chan operationResult
	editing           bool
	editorSuspended   bool
	applyEditorResult func(operationResult) // Captures the edited draft/question and tab.
	attachmentDone    chan attachmentResult
	attaching         bool
	cancelAttachment  context.CancelFunc

	retainedQuestion   *questionDialog
	deferredQuestionID string
	questionRow        int // Only the singleton live status row needs reconciliation; -1 means absent.
	pendingBTW         []session.Event
}

type frontendSubmission struct {
	text            string
	ready, steering bool
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
	c.done = make(chan operationResult, 1)
	c.onClose(c.stopForeground)
	c.queueGeneration = c.frontend.Runtime.Generation()
	c.view = newTranscript()
	c.seenPrompts = map[string]int64{}
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
	c.replay()
	c.ticker = time.NewTicker(250 * time.Millisecond)
	c.onClose(c.ticker.Stop)
	return nil
}

func (c *frontendController) setupTerminal() error {
	if c.frontend.Plain {
		return nil
	}
	c.screen, c.g = c.frontend.Screen, c.frontend.Graphics
	if c.screen == nil {
		var err error
		c.screen, c.tty, c.g, err = newTerminal(c.ctx)
		if err != nil {
			return err
		}
	}
	if err := c.screen.Init(); err != nil {
		return err
	}
	c.onClose(c.screen.Fini)
	if c.tty != nil && c.g != nil && c.screen.Colors() < 1<<24 {
		c.g = nil
		c.graphicsWarning = "24-bit color is disabled; Kitty math/images require RGB colors. Remove NO_COLOR or TCELL_TRUECOLOR=disable."
	} else if c.tty != nil && c.g == nil {
		c.graphicsWarning = "Kitty graphics unavailable; math stays as TeX."
		if c.tty.graphicsError != nil {
			c.graphicsWarning += " " + c.tty.graphicsError.Error()
		}
	}
	c.screen.EnableMouse(tcell.MouseButtonEvents)
	c.screen.EnablePaste()
	c.screen.SetCursorStyle(tcell.CursorStyleBlinkingBar)
	if c.tty != nil && c.g != nil && c.g.Tmux {
		activation, stopActivation, err := graphics.WatchTmuxActivation(c.ctx)
		c.graphicsActivation = activation
		if err != nil {
			c.graphicsWarning = "Tmux image recovery unavailable: " + err.Error()
		} else {
			c.onClose(stopActivation)
		}
	}
	if c.g != nil && !c.g.Tmux {
		c.screen.EnableFocus()
	}
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
		c.renderer, err = newImageRenderer(c.ctx, c.g)
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
	if c.busy {
		c.frontend.Runtime.CancelForeground()
		waitForTurn(c.done, c.frontend.Events)
	}
}
