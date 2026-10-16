package tui

import (
	"fmt"

	"github.com/gdamore/tcell/v3"
)

func (c *frontendController) handleKey(ev *tcell.EventKey) (next frontendSubmission) {
	if !ev.Pressed() {
		return next
	}
	if c.modal.window != nil && c.modal.window.completionEntry != 0 && ev.Key() == tcell.KeyRune && (ev.Str() == "d" || ev.Str() == "D") && ev.Modifiers() == 0 {
		c.inspectSaved(c.modal.window.completionEntry)
		return next
	}
	ev = normalizeFrontendKey(ev)
	if isCtrl(ev, "c") {
		if c.draft.text != "" {
			c.draft.set("")
			c.recall.index, c.recall.draft = len(c.recall.entries), ""
			c.esc, c.viewChord = false, false
			return next
		}
		next.text, next.ready = "/quit", true
		return next
	}
	if c.modal.prompts != nil {
		w, h := c.screen.Size()
		_, height := windowContentSize(w, h, c.modal.window)
		text, closed := c.modal.prompts.key(ev, height)
		if closed {
			c.modal.clear()
			if text != "" {
				c.recallInput(text)
				c.recall.index, c.recall.draft = len(c.recall.entries), ""
			}
		}
		return next
	}
	if c.draft.pasting && (c.pasteQuestion == nil || c.modal.question != c.pasteQuestion) {
		if c.pasteIntoComposer {
			c.modal.clear()
			c.draft.pasteKey(ev)
		}
		return next
	}
	if c.modal.preview != nil {
		close, confirm := c.modal.preview.key(ev)
		if close {
			c.finishPreview(confirm)
		}
		return next
	}
	if c.viewChord && ev.Key() == tcell.KeyRune && (ev.Str() == "e" || ev.Str() == "E") {
		c.viewChord = false
		c.startEditor()
		return next
	}
	if c.modal.question != nil {
		c.handleQuestionKey(ev)
		return next
	}
	if c.viewChord {
		if result, handled := c.handleViewChord(ev); handled {
			return result
		}
	}
	if isCtrl(ev, "x") {
		c.viewChord = true
		return next
	}
	if isCtrl(ev, "p") {
		c.modal.clear()
		c.modal.generation = c.frontend.Runtime.Generation()
		c.modal.commands = newCommandMenu()
		c.modal.window = &c.modal.commands.Window
		return next
	}
	if isCtrl(ev, "r") {
		c.modal.clear()
		c.setFullscreen(false)
		c.modal.generation = c.frontend.Runtime.Generation()
		c.modal.prompts = newPromptSearch(c.recall.entries)
		c.modal.window = &c.modal.prompts.Window
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
		_, height := windowContentSize(w, h, c.modal.window)
		if c.modal.window.Key(ev, height) {
			return next
		}
	}
	if c.sidebar.overlay && c.modal.window == nil && c.sidebar.key(ev) {
		return next
	}
	if c.handleCompletionKey(ev) {
		return next
	}
	if ev.Key() == tcell.KeyEscape {
		if c.modal.window != nil {
			c.modal.clear()
		} else if c.sidebar.overlay {
			c.sidebar.overlay = false
		} else if c.fullscreen {
			c.setFullscreen(false)
		} else if !c.view.followTail {
			c.view.followTail = true
		} else if c.esc {
			c.frontend.Runtime.Interrupt()
			c.esc = false
		} else {
			c.esc = true
		}
		return next
	}
	return c.handleComposerKey(ev)
}

func (c *frontendController) handleScreenEvent(ev tcell.Event) (next frontendSubmission) {
	switch ev := ev.(type) {
	case *tcell.EventPaste:
		c.handlePaste(ev)
	case *tcell.EventFocus:
		if ev.Focused && c.g != nil && !c.g.Tmux {
			c.g.Refresh()
			c.screen.Sync()
		}
	case *tcell.EventResize:
		c.screen.Sync()
	case *tcell.EventMouse:
		c.handleMouse(ev)
	case *tcell.EventKey:
		return c.handleKey(ev)
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
			if command != "" {
				c.draft.set(command)
				c.setFullscreen(false)
			}
		}
		return next, true
	}
	if c.modal.menu != nil {
		c.esc = false
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
				if _, err := c.frontend.Runtime.Jobs.Promote("main", id); err != nil {
					c.add("Background failed: "+err.Error(), 0)
				}
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
	if c.draft.completion != nil && len(c.completion.result.items) > 0 {
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
			for _, job := range foreground {
				if _, err := c.frontend.Runtime.Jobs.Promote("main", job.ID); err != nil {
					c.add("Background failed: "+err.Error(), 0)
				}
			}
			return next
		}
	}
	if c.draft.key(ev) {
		c.modal.clear()
		return next
	}
	switch {
	case isCtrl(ev, "d"):
		c.displayView().pageDown(len(c.displayView().visible))
	case isCtrl(ev, "u"):
		c.displayView().scroll(-max(1, len(c.displayView().visible)/2), len(c.displayView().visible))
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
		if target >= 0 && target < len(c.displayView().lines) && c.displayView().lines[target].id != 0 {
			c.inspect(c.displayView().lines[target].id)
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
		if c.draft.text == "" && target >= 0 && target < len(c.displayView().lines) && c.displayView().lines[target].id != 0 {
			c.inspect(c.displayView().lines[target].id)
		} else {
			next.text = c.draft.text
			next.steering = c.busy && !c.commandBusy && ev.Modifiers()&tcell.ModAlt == 0
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
	c.viewChord = false
	if !c.modal.question.pasting && c.modal.question.textEntry() && isCtrl(ev, "x") {
		c.viewChord = true
		return
	}
	w, h := c.screen.Size()
	_, height := windowContentSize(w, h, c.modal.window)
	answers, dismissed := c.modal.question.key(ev, height)
	if dismissed {
		if err := c.frontend.Runtime.DismissQuestion(c.modal.question.form.ID); err != nil {
			c.modal.question.errorText = err.Error()
			c.modal.question.update()
		} else {
			c.modal.clear()
			c.syncQuestionState(c.frontend.Runtime.PendingQuestion())
			c.esc = false
		}
	}
	if answers != nil {
		if err := c.frontend.Runtime.AnswerQuestion(c.modal.question.form.ID, answers); err != nil {
			c.modal.question.errorText = err.Error()
			c.modal.question.update()
		} else {
			c.retainedQuestion = nil
			c.modal.clear()
		}
	}

}

func (c *frontendController) handleViewChord(ev *tcell.EventKey) (next frontendSubmission, handled bool) {
	c.viewChord = false
	if ev.Key() == tcell.KeyRune && (ev.Str() == "f" || ev.Str() == "F") {
		c.setFullscreen(!c.fullscreen)
		c.sidebar.overlay = false
		return next, true
	}
	if ev.Key() == tcell.KeyRune && (ev.Str() == "s" || ev.Str() == "S") {
		c.sidebar.overlay = !c.sidebar.overlay
		return next, true
	}
	if ev.Key() == tcell.KeyRune && (ev.Str() == "m" || ev.Str() == "M") {
		c.openModelMenu()
		return next, true
	}
	if ev.Key() == tcell.KeyRune && (ev.Str() == "l" || ev.Str() == "L") {
		c.openSessionMenu()
		return next, true
	}
	if ev.Key() == tcell.KeyRune && (ev.Str() == "g" || ev.Str() == "G") {
		c.openHistoryMenu()
		return next, true
	}
	if ev.Key() == tcell.KeyRune {
		switch ev.Str() {
		case "?":
			c.openQuestions("")
			return next, true
		case "j", "J":
			next.text = "/jobs"
		case "t", "T":
			next.text = "/timers"
		case "n", "N":
			next.text = "/new"
		case "q", "Q":
			next.text = "/quit"
		}
		if next.text != "" {
			c.modal.clear()
			c.setFullscreen(false)
			next.ready = true
			return next, true
		}
	}

	return next, false
}

func (c *frontendController) handleMouse(ev *tcell.EventMouse) {
	if c.fullscreen {
		c.dirty = false
		return // Ignore reports already queued before terminal selection mode.
	}
	x, y := ev.Position()
	if c.modal.preview != nil {
		close, confirm := c.modal.preview.mouse(ev)
		if close {
			c.finishPreview(confirm)
		}
		return
	}
	if c.modal.window != nil {
		if c.modal.prompts != nil {
			w, h := c.screen.Size()
			c.modal.prompts.mouse(ev, w, h)
			return
		}
		if c.modal.background != nil {
			w, h := c.screen.Size()
			c.modal.background.mouse(ev, w, h)
			return
		}
		if c.modal.history != nil && c.modal.window == &c.modal.history.Window {
			w, h := c.screen.Size()
			c.modal.history.mouse(ev, w, h)
			return
		}
		if c.modal.sessions != nil {
			w, h := c.screen.Size()
			c.modal.sessions.mouse(ev, w, h)
		}
		if ev.Buttons()&tcell.WheelUp != 0 {
			c.modal.window.Scroll = max(0, c.modal.window.Scroll-3)
			if c.modal.question != nil {
				c.modal.question.manualScroll = true
			}
		}
		if ev.Buttons()&tcell.WheelDown != 0 {
			c.modal.window.Scroll += 3
			if c.modal.question != nil {
				c.modal.question.manualScroll = true
			}
		}
		return
	}
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
		return
	}
	if ev.Buttons()&tcell.WheelUp != 0 {
		c.view.scroll(-3, len(c.view.visible))
	}
	if ev.Buttons()&tcell.WheelDown != 0 {
		c.view.scroll(3, len(c.view.visible))
	}
	width, _ := c.screen.Size()
	width = c.sidebar.bounds(width, c.sidebar.height, c.fullscreen)
	if ev.Buttons()&tcell.Button1 != 0 && x >= 0 && x < width-1 {
		if id := c.view.entryAt(y); id != 0 {
			c.inspect(id)
		}
	}
}

func (c *frontendController) handlePaste(ev *tcell.EventPaste) {
	if c.modal.prompts != nil {
		c.modal.prompts.query.pasting = ev.Start()
		return
	}
	c.draft.pasting = ev.Start()
	if ev.Start() {
		c.esc, c.viewChord = false, false
		c.pasteQuestion = c.modal.question
		c.pasteIntoComposer = c.pasteQuestion == nil && c.modal.menu == nil && c.modal.commands == nil && c.modal.preview == nil
	}
	if c.pasteQuestion != nil {
		c.esc = false
		c.pasteQuestion.pasting = ev.Start()
		if ev.Start() && c.pasteQuestion.tab < len(c.pasteQuestion.answers) {
			c.pasteQuestion.manualScroll = false
			a := &c.pasteQuestion.answers[c.pasteQuestion.tab]
			a.choice = len(c.pasteQuestion.form.Questions[c.pasteQuestion.tab].Options)
			a.useCustom, a.editing = true, true
			c.pasteQuestion.update()
		}
	}
	if !ev.Start() {
		c.pasteQuestion, c.pasteIntoComposer = nil, false
	}
}
