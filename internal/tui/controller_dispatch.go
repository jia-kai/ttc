package tui

import (
	"ttc/internal/session"

	"github.com/gdamore/tcell/v3"
)

// handleScreenEvent is the root input boundary. Components receive normalized
// key presses only; paste payloads keep the destination captured at paste-start.
func (c *frontendController) handleScreenEvent(ev tcell.Event) (next frontendSubmission) {
	if _, key := ev.(*tcell.EventKey); !key {
		c.flushPaste()
	}
	switch ev := ev.(type) {
	case *tcell.EventPaste:
		c.handlePaste(ev)
	case *tcell.EventFocus:
		if ev.Focused && c.g != nil && !c.g.Tmux {
			refreshTerminalGraphics(c.screen, c.g, false)
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

func (c *frontendController) handleKey(ev *tcell.EventKey) (next frontendSubmission) {
	if !ev.Pressed() {
		return next
	}
	ev = normalizeFrontendKey(ev)
	if c.paste.active {
		if !isCtrl(ev, "c") {
			c.collectPasteKey(ev)
			if !c.batchingEvents {
				c.flushPaste()
			}
			return next
		}
		c.flushPaste() // Ctrl+C observes the partially pasted draft, just like typing.
	}
	if result, handled := c.handleGlobalKey(ev); handled {
		return result
	}
	// A focused dialog owns all remaining keys, including unrecognized ones.
	// They must not edit or submit an input hidden behind it.
	if !c.modal.empty() {
		return c.handleModalKey(ev)
	}
	if c.sidebar.overlay && !c.fullscreen {
		if ev.Key() == tcell.KeyEscape {
			c.sidebar.overlay = false
			return next
		}
		if c.sidebar.key(ev) {
			return next
		}
	}
	// Context scrolling belongs to the transcript, whether the bottom input is
	// the composer or a question. Dialogs retain their own scrolling bindings.
	if isCtrl(ev, "u") {
		c.esc = false
		view := c.displayView()
		view.scroll(-max(1, len(view.visible)/2), len(view.visible))
		return next
	}
	if isCtrl(ev, "d") {
		c.esc = false
		view := c.displayView()
		view.pageDown(len(view.visible))
		return next
	}
	if c.focusedQuestion() != nil {
		c.handleQuestionKey(ev)
		return next
	}
	if c.handleCompletionKey(ev) {
		return next
	}
	if ev.Key() == tcell.KeyEscape {
		if c.fullscreen {
			c.setFullscreen(false)
		} else if !c.view.followTail {
			c.view.followTail = true
		} else if c.esc {
			c.control(session.Request{Kind: session.RequestInterrupt})
			c.esc = false
		} else {
			c.esc = true
		}
		return next
	}
	return c.handleComposerKey(ev)
}

// handleGlobalKey runs before focused components, so no component can swallow
// application shortcuts. Paste payloads bypass it except for Ctrl+C.
func (c *frontendController) handleGlobalKey(ev *tcell.EventKey) (next frontendSubmission, handled bool) {
	if ev.Key() == tcell.KeyEscape && c.questionReopenRequest != 0 {
		// Escape supersedes an asynchronous reopen even before its acknowledgment.
		c.questionReopenRequest = 0
		if form := c.frontend.Runtime.PendingQuestion(); form != nil && c.question == nil {
			c.control(session.Request{Kind: session.RequestDismissQuestion, ID: form.ID})
		}
	}
	if isCtrl(ev, "c") {
		if c.draft.text != "" {
			c.draft.set("")
			c.recall.index, c.recall.draft = len(c.recall.entries), ""
			c.esc, c.viewChord = false, false
		} else {
			next.text, next.ready = "/quit", true
		}
		return next, true
	}
	if c.viewChord {
		if result, handled := c.handleViewChord(ev); handled {
			return result, true
		}
	}
	if isCtrl(ev, "x") {
		c.esc = false
		c.viewChord = true
		return next, true
	}
	if isCtrl(ev, "p") {
		c.setFullscreen(false)
		c.modal.clear()
		c.modal.generation = c.frontend.Runtime.Generation()
		c.modal.commands = newCommandMenu()
		c.modal.window = &c.modal.commands.Window
		return next, true
	}
	if isCtrl(ev, "r") && c.modal.prompts == nil {
		c.modal.clear()
		c.setFullscreen(false)
		c.modal.generation = c.frontend.Runtime.Generation()
		c.modal.prompts = newPromptSearch(c.recall.entries)
		c.modal.window = &c.modal.prompts.Window
		return next, true
	}
	return next, false
}

func (c *frontendController) handleViewChord(ev *tcell.EventKey) (next frontendSubmission, handled bool) {
	c.viewChord = false
	if ev.Key() == tcell.KeyEscape {
		return next, true
	}
	if ev.Key() != tcell.KeyRune || ev.Modifiers()&^tcell.ModShift != 0 {
		return next, false
	}
	switch ev.Str() {
	case "e", "E":
		c.startEditor()
	case "f", "F":
		c.modal.clear()
		c.setFullscreen(!c.fullscreen)
		c.sidebar.overlay = false
	case "s", "S":
		c.setFullscreen(false)
		c.modal.clear()
		c.sidebar.overlay = !c.sidebar.overlay
	case "m", "M":
		c.setFullscreen(false)
		c.openModelMenu()
	case "l", "L":
		c.setFullscreen(false)
		c.openSessionMenu()
	case "g", "G":
		c.setFullscreen(false)
		c.openHistoryMenu()
	case "?":
		c.openQuestions("")
	case "j", "J":
		next.text = "/jobs"
	case "t", "T":
		next.text = "/timers"
	case "n", "N":
		next.text = "/new"
	case "q", "Q":
		next.text = "/quit"
	default:
		return next, false
	}
	if next.text != "" {
		c.modal.clear()
		c.setFullscreen(false)
		next.ready = true
	}
	return next, true
}

// handleMouse follows visual ownership: copy mode ignores reports, dialogs
// capture them, then the sidebar and inline form get their own hit regions.
func (c *frontendController) handleMouse(ev *tcell.EventMouse) {
	if c.fullscreen {
		c.dirty = false
		return // Ignore reports already queued before terminal selection mode.
	}
	if c.handleModalMouse(ev) || c.handleSidebarMouse(ev) || c.handleQuestionMouse(ev) {
		return
	}
	c.handleTranscriptMouse(ev)
}
