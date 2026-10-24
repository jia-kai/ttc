package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/render"
	"ttc/internal/session"
)

func (c *frontendController) recallInput(text string) {
	c.draft.set(text)
	// A recalled slash command or @ path must not capture the next history key.
	if q, active := completionAt(c.draft, c.frontend.Runtime.Workspace.Root, c.frontend.Runtime.Generation()); active {
		c.completion.dismissed, c.completion.hasDismissed = q, true
	}
	c.hadQuery = false
}

func (c *frontendController) attach(path string) {
	if c.attaching {
		addError := "An attachment is still being prepared"
		c.view.append(line{text: addError})
		return
	}
	c.attaching = true
	generation, types := c.frontend.Runtime.Generation(), c.frontend.Runtime.CurrentSelection().Model.BinaryFileTypes()
	attachmentCtx, cancel := context.WithCancel(c.ctx)
	c.cancelAttachment = cancel
	path = c.frontend.Runtime.Workspace.Path(path)
	done := c.attachmentDone
	go func() {
		a, err := contextbuild.Snapshot(attachmentCtx, path, types)
		done <- attachmentResult{a, generation, err}
	}()
}

func (c *frontendController) startEditor() {
	text := c.draft.text
	apply := func(result operationResult) {
		if result.err != nil {
			c.add("Editor failed: "+result.err.Error(), 0)
		} else {
			c.draft.set(result.text)
		}
	}
	if question := c.modal.question; question != nil {
		if !question.textEntry() {
			return
		}
		tab, generation := question.tab, c.frontend.Runtime.Generation()
		text = question.answers[tab].custom.text
		apply = func(result operationResult) {
			pending := c.frontend.Runtime.PendingQuestion()
			if c.frontend.Runtime.Generation() != generation || pending == nil || pending.ID != question.form.ID {
				return // The editor cannot revive a closed question.
			}
			question.errorText = ""
			if result.err != nil {
				question.errorText = "Editor failed: " + result.err.Error()
			} else {
				next := question.answers[tab].custom
				next.set(result.text)
				if err := question.acceptText(tab, next); err != nil {
					question.errorText = err.Error()
				}
			}
			question.manualScroll = false
			question.update()
		}
	}
	if c.screen == nil {
		c.add("Editor requires the terminal UI", 0)
		return
	}
	if err := c.screen.Suspend(); err != nil {
		apply(operationResult{err: err})
		return
	}
	c.editing, c.editorSuspended = true, true
	c.applyEditorResult = apply
	editor := c.frontend.EditInput
	if editor == nil {
		editor = editDraft
	}
	ctx, done := c.ctx, c.editorDone
	go func() { text, err := editor(ctx, text); done <- operationResult{text: text, err: err} }()
}

func (c *frontendController) finishPreview(confirm bool) {
	if c.modal.preview != nil && c.modal.preview.pending {
		var point *[2]int
		if confirm {
			point = c.modal.preview.point
		}
		if err := c.frontend.Runtime.ConfirmImage(c.modal.preview.source.id, point); err != nil {
			c.add("Error: "+err.Error(), 0)
		} else {
			// Confirmation resumes the requesting agent; reveal its incoming reply.
			c.view.followTail = true
		}
	}
	parent := c.modal.parent
	c.modal.clear()
	if parent != nil {
		c.modal = *parent
	}
	c.view.invalidate()
}

func (c *frontendController) openLocalImage(asset placedImage) {
	parent := c.modal
	generation := c.frontend.Runtime.Generation()
	load, err := c.renderer.loadSource(imageSource{id: asset.key, path: asset.path, file: asset.path})
	if err != nil {
		c.modal = modalState{parent: &parent, generation: generation, window: &Window{Title: "Image", Text: "Image preview failed: " + err.Error()}}
		return
	}
	c.modal = modalState{parent: &parent, loading: &previewLoad{imageLoad: load, generation: generation}, generation: generation, window: &Window{Title: "Image", Text: "Loading full image…"}}
}

func (c *frontendController) openModelMenu() {
	c.esc, c.viewChord = false, false
	if len(c.frontend.Models) == 0 {
		c.add("Error: model catalog unavailable", 0)
		return
	}
	if c.frontend.Plain {
		var b strings.Builder
		for _, model := range c.frontend.Models {
			fmt.Fprintf(&b, "%s · %s · variants=%v · default=%s\n", model.ID, model.Name, model.Variants, model.DefaultVariant)
		}
		c.add(b.String()+"Select with /model ID [VARIANT]", 0)
		return
	}
	selected := c.frontend.Runtime.ModelChoice()
	c.modal.clear()
	c.modal.menu = newModelMenu(c.frontend.Models, selected)
	c.modal.question = nil
	c.modal.window = &c.modal.menu.Window
}

func (c *frontendController) openSessionMenu() {
	c.esc, c.viewChord = false, false
	if c.busy {
		c.add("Session picker requires an idle turn", 0)
		return
	}
	list, err := c.history.Sessions(c.frontend.Runtime.Workspace.Root)
	if err != nil {
		c.add("Sessions failed: "+err.Error(), 0)
		return
	}
	c.modal.clear()
	c.modal.sessions = newSessionMenu(list, c.frontend.Runtime.Current(), time.Now())
	c.modal.window = &c.modal.sessions.Window
}

func (c *frontendController) openHistoryMenu() {
	c.esc, c.viewChord = false, false
	if c.busy {
		c.add("History picker requires an idle turn", 0)
		return
	}
	tree, err := c.frontend.Runtime.History()
	if err != nil {
		c.add("History failed: "+err.Error(), 0)
		return
	}
	c.modal.clear()
	c.modal.history = newHistoryMenu(tree)
	c.modal.window = &c.modal.history.Window
}

func (c *frontendController) openBackgroundMenu() {
	if c.commandBusy {
		return
	}
	list := c.frontend.Runtime.Jobs.Foreground("main")
	if len(list) == 0 {
		return
	}
	c.modal.clear()
	c.modal.background = newBackgroundMenu(list)
	c.modal.window = &c.modal.background.Window
}

func (c *frontendController) selectModel(id, variant string) {
	selection, err := llm.Resolve(c.frontend.Runtime.CurrentSelection().Provider, c.frontend.Models, id, variant)
	if err != nil {
		c.add("Error: "+err.Error(), 0)
		return
	}
	if err := c.frontend.Runtime.RequestModel(selection); err != nil {
		c.add("Model selection failed: "+err.Error(), 0)
		return
	}
	c.notificationPaused = false
	c.add("Model selected for next tool boundary · "+id+" · "+selection.Variant, 0)
}

func (c *frontendController) submit(next frontendSubmission) bool {
	if !next.ready {
		return false
	}
	text := strings.TrimSpace(next.text)
	if text == "" {
		return false
	}
	if text != "/eof" {
		c.recall.add(next.text)
	}
	if text == "/quit" {
		c.frontend.Runtime.Interrupt()
		if c.busy {
			waitForTurn(c.done, c.frontend.Events)
			c.busy = false
		}
		return true
	}
	if text == "/eof" {
		c.eof = true
		return false
	}
	if strings.HasPrefix(text, "/") {
		c.submitCommand(text)
	} else {
		c.submitPrompt(next)
	}
	return false
}

func (c *frontendController) submitCommand(text string) {
	if text == "/cancel-queue" || text == "/cancel-steer" {
		c.cancelPendingInput(text)
		return
	}
	if text == "/editor" {
		c.startEditor()
		return
	}
	if text == "/help" {
		c.showHelp()
		return
	}
	if text == "/jobs" || text == "/timers" {
		c.showRuntimeSnapshot(text)
		return
	}
	if text == "/btw" || strings.HasPrefix(text, "/btw ") {
		if c.commandBusy {
			c.add("Error: wait for the current history command before /btw", 0)
		} else if _, err := c.frontend.Runtime.StartBTW(strings.TrimSpace(strings.TrimPrefix(text, "/btw"))); err != nil {
			c.add("Error: "+err.Error(), 0)
		}
		return
	}
	if strings.HasPrefix(text, "/answer ") {
		c.answerQuestion(text)
		return
	}
	if strings.HasPrefix(text, "/inspect ") {
		id, e := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(text, "/inspect ")), 10, 64)
		if e != nil {
			c.add("Error: invalid entry ID", 0)
		} else {
			c.inspectSaved(id) // Explicit entry inspection retains raw completion/tool metadata.
		}
		return
	}
	if text == "/questions" || strings.HasPrefix(text, "/questions ") {
		c.openQuestions(strings.TrimSpace(strings.TrimPrefix(text, "/questions")))
		return
	}
	if strings.HasPrefix(text, "/attach ") {
		c.attach(strings.TrimSpace(strings.TrimPrefix(text, "/attach ")))
		return
	}
	if text == "/models" || text == "/model" {
		c.openModelMenu()
		return
	}
	if strings.HasPrefix(text, "/model ") {
		c.requestModel(text)
		return
	}
	if text == "/login" {
		c.login()
		return
	}
	if text == "/sessions" && !c.frontend.Plain {
		c.openSessionMenu()
		return
	}
	if text == "/history" && !c.frontend.Plain {
		c.openHistoryMenu()
		return
	}
	if text == "/background" {
		c.background()
		return
	}
	c.startCommand(text)
}

func (c *frontendController) submitPrompt(next frontendSubmission) {
	pending := contextbuild.Input{Text: next.text, Attachments: c.attachments}
	if redirected, err := c.frontend.Runtime.RedirectDismissedQuestion(pending); err != nil {
		c.draft.set(next.text)
		c.add("Redirect failed: "+err.Error(), 0)
		return
	} else if redirected {
		c.attachments = nil
		return
	}
	if c.frontend.Runtime.Awaiting("main") {
		// A newly submitted input must not overtake FIFO input accepted before
		// the awaiting transition. Publish the whole batch before waking inference.
		if err := c.steerQueuedInputs(pending); err != nil {
			c.draft.set(next.text)
			c.add("Steering failed: "+err.Error(), 0)
		} else {
			c.attachments = nil
		}
		return
	}
	if next.steering {
		if err := c.frontend.Runtime.Steer(pending); err != nil {
			c.draft.set(next.text)
			c.add("Steering failed: "+err.Error(), 0)
		} else {
			c.attachments = nil
		}
		return
	}
	c.attachments = nil
	pending.Source = "normal"
	if c.busy || len(c.queue) > 0 {
		pending.Source = "queue"
	}
	c.queue = append(c.queue, pending)
	if c.busy {
		c.add(fmt.Sprintf("Queued input · %d unsent", len(c.queue)), 0)
	}
}

// steerQueuedInputs transfers queue ownership only after runtime acceptance.
// It runs solely on the frontend event loop, never starts another Run, and
// retains original input units rather than flattening text or attachments.
func (c *frontendController) steerQueuedInputs(extra ...contextbuild.Input) error {
	inputs := make([]contextbuild.Input, 0, len(c.queue)+len(extra))
	inputs = append(inputs, c.queue...)
	inputs = append(inputs, extra...)
	if err := c.frontend.Runtime.SteerInputs(inputs); err != nil {
		return err
	}
	clear(c.queue)
	c.queue = nil
	return nil
}

func (c *frontendController) cancelPendingInput(text string) {
	var restored contextbuild.Input
	var err error
	if text == "/cancel-queue" {
		if len(c.queue) == 0 {
			if c.pendingInput == nil {
				err = errors.New("no queued prompt to cancel")
			} else {
				restored, err = c.pendingInput.Cancel()
			}
		} else {
			last := len(c.queue) - 1
			restored = c.queue[last]
			c.queue[last] = contextbuild.Input{}
			c.queue = c.queue[:last]
		}
	} else {
		restored, err = c.frontend.Runtime.CancelSteer()
	}
	if err != nil {
		c.add("Error: "+err.Error(), 0)
	} else {
		c.recallInput(restored.Text)
		c.attachments = restored.Attachments
		c.add("Cancelled pending input · restored to composer", 0)
	}
}

func (c *frontendController) showHelp() {
	result, err := c.frontend.Runtime.Command("/help")
	if err != nil {
		c.add("Error: "+err.Error(), 0)
	} else if c.frontend.Plain {
		fmt.Fprintln(c.frontend.Output, result)
	} else {
		c.modal.clear()
		c.modal.generation = c.frontend.Runtime.Generation()
		c.modal.window = &Window{Title: "Command result", Text: result, Markdown: true}
	}
}

func (c *frontendController) showRuntimeSnapshot(text string) {
	var snapshot any = c.frontend.Runtime.LiveTimers()
	if text == "/jobs" {
		snapshot = c.frontend.Runtime.Jobs.Metadata("main")
	}
	body, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		c.add("Inspection failed: "+err.Error(), 0)
	} else if c.frontend.Plain {
		fmt.Fprintln(c.frontend.Output, string(body))
	} else {
		c.modal.clear()
		c.modal.generation = c.frontend.Runtime.Generation()
		c.modal.window = &Window{Title: strings.TrimPrefix(text, "/"), Text: render.Fence(string(body), "json"), Markdown: true}
	}
}

func (c *frontendController) answerQuestion(text string) {
	fields := strings.SplitN(strings.TrimPrefix(text, "/answer "), " ", 2)
	if len(fields) != 2 {
		c.add("Error: /answer FORM_ID JSON_ARRAY", 0)
		return
	}
	var answers []session.Answer
	err := json.Unmarshal([]byte(fields[1]), &answers)
	if err == nil {
		err = c.frontend.Runtime.AnswerQuestion(fields[0], answers)
	}
	if err != nil {
		c.add("Error: "+err.Error(), 0)
	}
}

func (c *frontendController) requestModel(text string) {
	fields := strings.Fields(text)
	if len(fields) < 2 || len(fields) > 3 {
		c.add("Error: /model ID [VARIANT]", 0)
		return
	}
	variant := ""
	if len(fields) == 3 {
		variant = fields[2]
	}
	c.selectModel(fields[1], variant)
}

func (c *frontendController) login() {
	if c.busy {
		c.add("Login requires an idle turn", 0)
		return
	}
	if c.screen != nil {
		c.add("Run ttc --login for device authorization", 0)
		return
	}
	if c.frontend.Login == nil {
		c.add("Login requires plain mode: ttc --login", 0)
		return
	}
	// Authorization and discovery belong to the injected lifecycle owner.
	// Keep the frozen active selection, but never expose old-account choices.
	c.frontend.Models = nil
	models, err := c.frontend.Login(c.ctx)
	if err != nil {
		c.add("Error: authorization or model discovery failed: "+err.Error(), 0)
	} else {
		c.frontend.Models = models
		c.add("Login complete; use /model to select from the current account's catalog", 0)
	}
}

func (c *frontendController) background() {
	if c.commandBusy {
		c.add("Background requires the current history command to finish", 0)
		return
	}
	if c.frontend.Plain {
		for _, job := range c.frontend.Runtime.Jobs.Foreground("main") {
			if _, err := c.frontend.Runtime.Jobs.Promote("main", job.ID); err != nil {
				c.add("Background failed: "+err.Error(), 0)
			}
		}
	} else {
		c.openBackgroundMenu()
	}
}

func (c *frontendController) startCommand(text string) {
	if c.busy {
		c.add("Command requires an idle turn; Esc twice interrupts work, Ctrl+C clears input or exits if empty", 0)
		return
	}
	c.busy = true
	c.commandBusy = true
	c.started = time.Now()
	// Lifecycle commands join callbacks; keep draining their UI events while they wait.
	runtime, done := c.frontend.Runtime, c.done
	go func() {
		result, err := runtime.Command(text)
		command := strings.Fields(text)[0]
		markdown := command == "/compact" || command == "/force-compact"
		if json.Valid([]byte(result)) {
			result = render.Fence(result, "json")
			markdown = true
		}
		done <- operationResult{text: result, command: command, markdown: markdown, err: err}
	}()
}
