package tui

import (
	"context"
	"encoding/json"
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
	// The explicit global editor shortcut edits the active bottom input while
	// preserving any inspector or picker to return to after the editor closes.
	if question := c.question; question != nil && !c.fullscreen {
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
		if c.control(session.Request{Kind: session.RequestConfirmImage, ID: c.modal.preview.source.id, Point: point}) {
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
		c.add(b.String()+"Select main: /model main ID [VARIANT] · easy: /model easy ID [VARIANT] · clear easy: /model easy off", 0)
		return
	}
	c.modal.clear()
	current := c.frontend.Runtime.ModelChoice()
	if current.Provider != c.frontend.Runtime.ProviderID() {
		current = llm.Selection{}
	}
	c.modal.menu = newModelMenu(c.frontend.Models, current)
	c.modal.menu.easy = c.frontend.Runtime.EasyModelChoice()
	c.modal.menu.update()
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
	selection, err := llm.Resolve(c.frontend.Runtime.ProviderID(), c.frontend.Models, id, variant)
	if err != nil {
		c.add("Error: "+err.Error(), 0)
		return
	}
	c.control(session.Request{Kind: session.RequestModel, Selection: &selection})
}

func (c *frontendController) selectEasyModel(id, variant string) {
	selection, err := llm.Resolve(c.frontend.Runtime.ProviderID(), c.frontend.Models, id, variant)
	if err != nil {
		c.add("Easy model selection failed: "+err.Error(), 0)
		return
	}
	c.control(session.Request{Kind: session.RequestEasyModel, Selection: &selection})
}

func (c *frontendController) submit(next frontendSubmission) bool {
	if !next.ready {
		return false
	}
	if next.generation == 0 {
		next.generation, next.operationID = c.observedGeneration, c.observedOperationID
	}
	text := strings.TrimSpace(next.text)
	if text == "" {
		return false
	}
	if text != "/eof" {
		c.recall.add(next.text)
	}
	if text == "/quit" {
		c.control(session.Request{Kind: session.RequestInterrupt})
		return true
	}
	if text == "/eof" {
		c.control(session.Request{Kind: session.RequestEOF})
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
	if text == "/force-reconnect" || strings.HasPrefix(text, "/force-reconnect ") {
		switch {
		case text != "/force-reconnect":
			c.add("Usage: /force-reconnect", 0)
		case c.frontend.Reconnect == nil:
			c.add("Error: provider does not support reconnecting", 0)
		default:
			c.control(session.Request{Kind: session.RequestReconnect})
		}
		return
	}
	if text == "/jobs" || text == "/timers" {
		c.showRuntimeSnapshot(text)
		return
	}
	if text == "/btw" || strings.HasPrefix(text, "/btw ") {
		if c.commandBusy {
			c.add("Error: wait for the current history command before /btw", 0)
		} else {
			c.control(session.Request{Kind: session.RequestBTW, Command: strings.TrimSpace(strings.TrimPrefix(text, "/btw"))})
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
	request := session.Request{Kind: session.RequestInput, Generation: next.generation, OperationID: next.operationID, Input: pending, Steer: next.steering}
	if _, err := c.coordinator.Submit(request); err != nil {
		c.recallInput(next.text)
		c.add("Input was not accepted: "+err.Error(), 0)
		return
	}
	c.attachments = nil
	c.attachmentEpoch++
}

// request captures the last rendered execution, not an unseen runtime snapshot.
// Submission is nonblocking; actual control mutation belongs to the coordinator.
func (c *frontendController) request(request session.Request) error {
	request.Generation = c.observedGeneration
	request.OperationID = c.observedOperationID
	cancellation := request.Kind == session.RequestCancelQueue || request.Kind == session.RequestCancelSteer
	if cancellation && len(c.pendingRestores) >= 64 {
		return session.ErrCoordinatorFull
	}
	id, err := c.coordinator.Submit(request)
	if err == nil && request.Kind == session.RequestReopenQuestion {
		c.questionReopenRequest = id
	}
	if err == nil && cancellation {
		if c.pendingRestores == nil {
			c.pendingRestores = make(map[uint64]restoreBaseline)
		}
		c.pendingRestores[id] = restoreBaseline{draftRevision: c.draft.revision, attachmentEpoch: c.attachmentEpoch, attachmentCount: len(c.attachments)}
	}
	if err == nil && c.frontend.Plain {
		switch request.Kind {
		case session.RequestLogin, session.RequestModel, session.RequestEasyModel, session.RequestCommand, session.RequestReconnect:
			c.plainGateRequest = id
		}
	}
	return err
}

func (c *frontendController) control(request session.Request) bool {
	if err := c.request(request); err != nil {
		c.add("Control was not accepted: "+err.Error(), 0)
		return false
	}
	return true
}

func (c *frontendController) cancelPendingInput(text string) {
	kind := session.RequestCancelQueue
	if text == "/cancel-steer" {
		kind = session.RequestCancelSteer
	}
	c.control(session.Request{Kind: kind})
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
	if err != nil {
		c.add("Error: "+err.Error(), 0)
		return
	}
	c.control(session.Request{Kind: session.RequestAnswer, ID: fields[0], Answers: answers})
}

func (c *frontendController) requestModel(text string) {
	fields := strings.Fields(text)
	args := fields[1:]
	easy := len(args) > 0 && args[0] == "easy"
	if easy || (len(args) > 0 && args[0] == "main") {
		args = args[1:]
	}
	if easy && len(args) == 1 && args[0] == "off" {
		c.control(session.Request{Kind: session.RequestEasyModel})
		return
	}
	if easy && len(args) > 0 && args[0] == "--" {
		args = args[1:]
	} else if easy && len(args) > 0 && args[0] == "off" {
		args = nil
	}
	if len(args) < 1 || len(args) > 2 {
		c.add("Error: /model [main] ID [VARIANT] or /model easy [--] ID [VARIANT] or /model easy off", 0)
		return
	}
	variant := ""
	if len(args) == 2 {
		variant = args[1]
	}
	if easy {
		c.selectEasyModel(args[0], variant)
	} else {
		c.selectModel(args[0], variant)
	}
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
	if c.control(session.Request{Kind: session.RequestLogin}) {
		// Keep the active selection but hide old-account catalog choices while
		// the application-owned authorization effect runs off the UI loop.
		c.frontend.Models = nil
	}
}

func (c *frontendController) background() {
	if c.commandBusy {
		c.add("Background requires the current history command to finish", 0)
		return
	}
	if c.frontend.Plain {
		c.control(session.Request{Kind: session.RequestBackground})
	} else {
		c.openBackgroundMenu()
	}
}

func (c *frontendController) startCommand(text string) {
	c.control(session.Request{Kind: session.RequestCommand, Command: text})
}
