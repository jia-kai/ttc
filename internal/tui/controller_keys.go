package tui

import (
	"fmt"
	"ttc/internal/session"

	"github.com/gdamore/tcell/v3"
)

func (c *frontendController) handleModalKey(ev *tcell.EventKey) (next frontendSubmission) {
	if c.modal.window != nil && c.modal.window.completionEntry != 0 && ev.Key() == tcell.KeyRune && (ev.Str() == "d" || ev.Str() == "D") && ev.Modifiers() == 0 {
		c.inspectSaved(c.modal.window.completionEntry)
		return next
	}
	if c.modal.window != nil && c.modal.window.boundaryRaw != 0 && ev.Key() == tcell.KeyRune && (ev.Str() == "r" || ev.Str() == "R") && ev.Modifiers() == 0 {
		c.openBoundaryRaw(c.modal.window.boundaryRaw)
		return next
	}
	if c.modal.window != nil && c.modal.window.boundaryRaw != 0 && ev.Key() == tcell.KeyRune && (ev.Str() == "e" || ev.Str() == "E") && ev.Modifiers() == 0 {
		c.toggleRequestExpansion()
		return next
	}
	if c.modal.window != nil && c.modal.raw != nil && ev.Key() == tcell.KeyRune && ev.Modifiers() == 0 {
		switch ev.Str() {
		case "n", "N":
			c.navigateBoundaryRaw(1)
			return next
		case "p", "P":
			c.navigateBoundaryRaw(-1)
			return next
		}
	}
	if c.modal.prompts != nil {
		w, h := c.screen.Size()
		_, height := windowContentSize(w, h, c.modal.window)
		text, closed := c.modal.prompts.key(ev, height)
		if closed {
			c.modal.clear()
			if text != "" && c.revealComposer() {
				c.recallInput(text)
				c.recall.index, c.recall.draft = len(c.recall.entries), ""
			}
		}
		return next
	}
	if ev.Key() == tcell.KeyEscape && c.modal.parent != nil && (c.modal.preview == nil || c.modal.preview.details == nil) {
		c.finishPreview(false)
		return next
	}
	if c.modal.preview != nil {
		close, confirm := c.modal.preview.key(ev)
		if close {
			c.finishPreview(confirm)
		}
		return next
	}
	if picked, handled := c.handlePickerKey(ev); handled {
		return picked
	}
	if c.modal.window != nil {
		if ev.Key() == tcell.KeyEscape {
			if c.modal.history != nil {
				c.modal.window = &c.modal.history.Window
			} else {
				c.modal.clear()
			}
			c.esc = false
			return next
		}
		w, h := c.screen.Size()
		width, height := windowContentSize(w, h, c.modal.window)
		c.modal.window.currentLines(width)
		c.modal.window.Key(ev, height)
	}
	return next
}

func (c *frontendController) handlePickerKey(ev *tcell.EventKey) (next frontendSubmission, handled bool) {
	if c.modal.commands != nil {
		w, h := c.screen.Size()
		_, height := windowContentSize(w, h, c.modal.window)
		command, closed := c.modal.commands.key(ev, height)
		if closed {
			c.modal.clear()
			if command != "" && c.revealComposer() {
				c.draft.set(command)
				c.setFullscreen(false)
			}
		}
		return next, true
	}
	if c.modal.menu != nil {
		c.esc = false
		m := c.modal.menu
		if m.variants && ev.Key() == tcell.KeyRune && ev.Modifiers() == 0 && (ev.Str() == "e" || ev.Str() == "E") {
			model := m.models[m.modelIndex]
			variant := model.Variants[m.variantIndex]
			if m.easy != nil && m.easy.Model.ID == model.ID && m.easy.Variant == variant {
				c.control(session.Request{Kind: session.RequestEasyModel})
			} else {
				c.selectEasyModel(model.ID, variant)
			}
			m.easy = c.frontend.Runtime.EasyModelChoice()
			m.update()
			return next, true
		}
		w, h := c.screen.Size()
		_, height := windowContentSize(w, h, c.modal.window)
		id, variant, closed := c.modal.menu.key(ev, height)
		if closed {
			c.modal.clear()
			if id != "" {
				c.selectModel(id, variant)
			}
		}
		return next, true
	}
	if c.modal.background != nil {
		id, closed := c.modal.background.key(ev)
		if closed {
			c.modal.clear()
			if id != "" {
				c.control(session.Request{Kind: session.RequestBackground, ID: id})
			}
		}
		return next, true
	}
	if c.modal.sessions != nil {
		c.esc = false
		w, h := c.screen.Size()
		_, height := windowContentSize(w, h, c.modal.window)
		id, closed := c.modal.sessions.key(ev, height)
		if closed {
			c.modal.clear()
			if id != "" {
				next.text = "/load " + id
				next.ready = true
				c.setFullscreen(false)
			}
		}
		if !next.ready {
			return next, true
		}
		return next, true // Selected reload owns Enter; preserve the composer draft.
	}
	if c.modal.history != nil && c.modal.window == &c.modal.history.Window {
		c.esc = false
		w, h := c.screen.Size()
		_, height := windowContentSize(w, h, c.modal.window)
		action, id := c.modal.history.key(ev, height)
		switch action {
		case "close":
			c.modal.clear()
		case "inspect":
			c.inspect(id)
		case "restore":
			c.modal.clear()
			next.text, next.ready = fmt.Sprintf("/branch %d", id), true
			c.setFullscreen(false)
		}
		if !next.ready {
			return next, true
		}
		return next, true // Preserve the composer while selecting a branch.
	}

	return next, false
}

func (c *frontendController) handleCompletionKey(ev *tcell.EventKey) bool {
	if ev.Key() == tcell.KeyEnter && ev.Modifiers()&tcell.ModShift != 0 {
		return false // Shift+Enter always inserts a newline, never a completion.
	}
	if c.draft.completion != nil && len(c.completion.result.items) > 0 {
		if ev.Key() == tcell.KeyUp || ev.Key() == tcell.KeyDown {
			plainPathArrow := ev.Modifiers() == 0 && c.completion.result.query.marker == '@'
			if ev.Modifiers() != tcell.ModShift && !plainPathArrow {
				return false // Slash suggestions preserve history; Alt arrows retain view focus.
			}
		}
		switch ev.Key() {
		case tcell.KeyUp:
			c.completion.selected = max(0, c.completion.selected-1)
			return true
		case tcell.KeyDown:
			c.completion.selected = min(len(c.completion.result.items)-1, c.completion.selected+1)
			return true
		case tcell.KeyEscape:
			c.completion.dismissed, c.completion.hasDismissed = c.completion.result.query, true
			c.hadQuery = false
			return true
		case tcell.KeyTab, tcell.KeyEnter:
			q := c.completion.result.query
			exactCommand := q.marker == '/' && "/"+q.prefix == c.completion.result.items[c.completion.selected].value
			if ev.Key() != tcell.KeyEnter || !exactCommand {
				if path, ok := c.completion.accept(&c.draft); ok && path != "" {
					c.attach(path)
				}
				return true
			}
		}
	}

	return false
}

func (c *frontendController) handleComposerKey(ev *tcell.EventKey) (next frontendSubmission) {
	c.esc = false
	if isCtrl(ev, "b") && c.modal.empty() && !c.commandBusy {
		foreground := c.frontend.Runtime.Jobs.Foreground("main")
		if len(foreground) > 0 {
			c.control(session.Request{Kind: session.RequestBackground})
			return next
		}
	}
	if isCtrl(ev, "j") || isCtrl(ev, "k") {
		delta := 1
		if isCtrl(ev, "k") {
			delta = -1
		}
		c.scrollInput(delta)
		return next
	}
	if c.draft.key(ev) {
		c.modal.clear()
		return next
	}
	switch {
	case ev.Key() == tcell.KeyUp:
		if ev.Modifiers()&tcell.ModAlt != 0 {
			if c.fullscreen {
				c.copyFocused = max(0, c.copyFocused-1)
			} else {
				c.focused = max(0, c.focused-1)
			}
		} else {
			c.recallInput(c.recall.move(c.draft.text, -1))
		}
	case ev.Key() == tcell.KeyDown:
		if ev.Modifiers()&tcell.ModAlt != 0 {
			if c.fullscreen {
				c.copyFocused = min(len(c.copyView.lines)-1, c.copyFocused+1)
			} else {
				c.focused = min(len(c.view.lines)-1, c.focused+1)
			}
		} else {
			c.recallInput(c.recall.move(c.draft.text, 1))
		}
	case ev.Key() == tcell.KeyTab:
		target := c.focused
		if c.fullscreen {
			target = c.copyFocused
		}
		if target >= 0 && target < len(c.displayView().lines) {
			c.openRow(c.displayView().lines[target])
		}
	case ev.Key() == tcell.KeyEnter:
		if c.attaching {
			c.add("Preparing attachment; press Enter when ready", 0)
			return next
		}
		target := c.focused
		if c.fullscreen {
			target = c.copyFocused
		}
		if c.draft.text == "" && target >= 0 && target < len(c.displayView().lines) && c.displayView().lines[target].action != actionNone {
			c.openRow(c.displayView().lines[target])
		} else {
			next.text = c.draft.text
			next.generation, next.operationID = c.observedGeneration, c.observedOperationID
			next.steering = c.observedOperationID != 0 && !c.observedCommandBusy && ev.Modifiers()&tcell.ModAlt == 0
			c.setFullscreen(false)
			c.draft.set("")
			next.ready = true
			c.view.followTail = true
		}
	}

	return next
}

func normalizeFrontendKey(ev *tcell.EventKey) *tcell.EventKey {
	// Legacy tty Alt+Enter is ESC CR; tcell decodes it as Alt+Ctrl+M.
	if ev.Key() == tcell.KeyRune && (ev.Str() == "m" || ev.Str() == "j") && ev.Modifiers()&(tcell.ModAlt|tcell.ModCtrl) == tcell.ModAlt|tcell.ModCtrl {
		ev = tcell.NewEventKey(tcell.KeyEnter, "", tcell.ModAlt)
	}
	// Ctrl-H/I/M retain their Backspace/Tab/Enter meanings when
	// advanced reporting exposes the actual control-letter chord.
	if ev.Key() == tcell.KeyRune && ev.Modifiers()&tcell.ModCtrl != 0 && ev.Modifiers()&tcell.ModAlt == 0 {
		switch ev.Str() {
		case "h":
			ev = tcell.NewEventKey(tcell.KeyBackspace, "", ev.Modifiers()&^tcell.ModCtrl)
		case "i":
			ev = tcell.NewEventKeyEx(tcell.KeyTab, "", ev.Modifiers()&^tcell.ModCtrl, true, ev.Physical(), ev.Repeat())
		case "m":
			ev = tcell.NewEventKey(tcell.KeyEnter, "", ev.Modifiers()&^tcell.ModCtrl)
		}
	}

	return ev
}

func (c *frontendController) handleQuestionKey(ev *tcell.EventKey) {
	c.esc = false
	d := c.question
	geometry := c.layoutQuestionInput()
	d.Window.currentLines(geometry.bodyWidth)
	answers, dismissed := d.key(ev, geometry.bodyHeight)
	if dismissed {
		if err := c.request(session.Request{Kind: session.RequestDismissQuestion, ID: d.form.ID}); err != nil {
			d.errorText = err.Error()
			d.update()
		} else {
			c.question = nil
			c.syncQuestionState(c.frontend.Runtime.PendingQuestion())
			c.esc = false
		}
	}
	if answers != nil {
		if err := c.request(session.Request{Kind: session.RequestAnswer, ID: d.form.ID, Answers: answers}); err != nil {
			d.errorText = err.Error()
			d.update()
		} else {
			c.retainedQuestion = nil
			c.question = nil
		}
	}

}

func (c *frontendController) handleModalMouse(ev *tcell.EventMouse) bool {
	x, y := ev.Position()
	if c.modal.preview != nil {
		close, confirm := c.modal.preview.mouse(ev)
		if close {
			c.finishPreview(confirm)
		}
		return true
	}
	if c.modal.window != nil {
		if c.renderer != nil && ev.Buttons()&tcell.Button1 != 0 {
			w, h := c.screen.Size()
			left, top, _, outerHeight := windowBounds(w, h)
			bodyWidth, bodyHeight := windowContentSize(w, h, c.modal.window)
			header := len(c.modal.window.HeaderLines(bodyWidth, max(0, outerHeight-3)))
			bodyY := y - top - 1 - header
			if bodyY >= 0 && bodyY < bodyHeight {
				if asset := c.modal.window.imageAt(x-left-1, bodyY); asset != nil {
					c.openLocalImage(*asset)
					return true
				}
			}
		}
		if c.modal.prompts != nil {
			w, h := c.screen.Size()
			c.modal.prompts.mouse(ev, w, h)
			return true
		}
		if c.modal.background != nil {
			w, h := c.screen.Size()
			c.modal.background.mouse(ev, w, h)
			return true
		}
		if c.modal.history != nil && c.modal.window == &c.modal.history.Window {
			w, h := c.screen.Size()
			c.modal.history.mouse(ev, w, h)
			return true
		}
		if c.modal.sessions != nil {
			w, h := c.screen.Size()
			c.modal.sessions.mouse(ev, w, h)
		}
		delta := 0
		if ev.Buttons()&tcell.WheelUp != 0 {
			delta = -3
		} else if ev.Buttons()&tcell.WheelDown != 0 {
			delta = 3
		}
		if delta != 0 {
			w, h := c.screen.Size()
			width, height := windowContentSize(w, h, c.modal.window)
			c.modal.window.currentLines(width)
			c.modal.window.ScrollBy(delta, height)
			// Preserve the nonpaged wheel's immediate top clamp.
			c.modal.window.Scroll = max(0, c.modal.window.Scroll)
		}
		return true
	}
	return false
}

func (c *frontendController) handleSidebarMouse(ev *tcell.EventMouse) bool {
	if consumed, action := c.sidebar.mouse(ev); consumed {
		if action.workspace {
			c.modal.clear()
			c.modal.window = &Window{Title: "Session & workspace", Text: c.sidebar.detail()}
		}
		if action.jobID != "" && !c.commandBusy {
			c.modal.clear()
			c.modal.generation = c.frontend.Runtime.Generation()
			c.modal.window = &Window{Title: "Running job", Text: c.frontend.Runtime.JobDetail(action.jobID, ""), Markdown: true, JobID: action.jobID}
			if job, err := c.frontend.Runtime.Jobs.View("main", action.jobID); err == nil {
				c.sourceWindow(c.modal.window, job.Owner)
			} else {
				c.modal.window.Text = "Job inspection failed: " + err.Error()
			}
		}
		if action.timerID != "" && !c.commandBusy {
			c.modal.clear()
			c.modal.generation = c.frontend.Runtime.Generation()
			c.modal.window = &Window{Title: "Timer", Markdown: true, TimerID: action.timerID}
			if detail, err := c.frontend.Runtime.TimerDetail(action.timerID); err == nil {
				c.modal.window.Text, c.modal.window.Header = detail.Text, detail.Header
				c.modal.window.HeaderFocus = detail.Focus
				c.sourceWindow(c.modal.window, detail.Actor)
			} else {
				c.modal.window.Text = "Timer inspection failed: " + err.Error()
			}
		}
		return true
	}
	return false
}

func (c *frontendController) handleQuestionMouse(ev *tcell.EventMouse) bool {
	if c.question == nil {
		return false
	}
	x, y := ev.Position()
	geometry := c.layoutQuestionInput()
	// Status and queued-input rows are neither form nor transcript content.
	if x < 0 || x > geometry.bodyWidth || y < geometry.transcriptHeight {
		return false
	}
	_, height := c.screen.Size()
	if y >= geometry.headerY && y < height-1 {
		delta := mouseWheelDelta(ev)
		if delta != 0 {
			d := c.question
			d.Window.currentLines(geometry.bodyWidth)
			d.Window.ScrollBy(delta, geometry.bodyHeight)
			d.Window.Scroll = max(0, d.Window.Scroll)
			d.manualScroll = true
		}
	}
	return true
}

func mouseWheelDelta(ev *tcell.EventMouse) int {
	if ev.Buttons()&tcell.WheelUp != 0 {
		return -3
	}
	if ev.Buttons()&tcell.WheelDown != 0 {
		return 3
	}
	return 0
}

func (c *frontendController) handleTranscriptMouse(ev *tcell.EventMouse) {
	x, y := ev.Position()
	width, _ := c.screen.Size()
	width = c.sidebar.bounds(width, c.sidebar.height, c.fullscreen)
	if x < 0 || x >= width || y < 0 || y >= len(c.view.visible) {
		return
	}
	if delta := mouseWheelDelta(ev); delta != 0 {
		c.view.scroll(delta, len(c.view.visible))
	}
	if ev.Buttons()&tcell.Button1 != 0 && x < width-1 {
		if c.renderer != nil {
			if asset := imageAt(c.view.visible[y].assets, x); asset != nil {
				c.openLocalImage(*asset)
				return
			}
		}
		c.openRow(c.view.visible[y])
	}
}

func (c *frontendController) handlePaste(ev *tcell.EventPaste) {
	c.flushPaste()
	c.paste = pasteBatch{active: ev.Start(), fullscreen: c.fullscreen}
	c.draft.pasting = ev.Start()
	if !ev.Start() {
		return
	}
	c.esc, c.viewChord = false, false
	switch {
	case c.modal.prompts != nil:
		c.paste.search = c.modal.prompts
	case c.focusedQuestion() != nil:
		d := c.focusedQuestion()
		c.paste.question, c.paste.tab = d, d.tab
		if d.tab < len(d.answers) {
			d.manualScroll = false
			a := &d.answers[d.tab]
			a.choice = len(d.form.Questions[d.tab].Options)
			a.useCustom, a.editing = true, true
			d.update()
		}
	case c.modal.empty():
		c.paste.composer = true
	}
}
