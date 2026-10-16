package tui

import (
	"fmt"
	"time"

	"github.com/gdamore/tcell/v3"
)

func (c *frontendController) displayView() *transcript {
	if c.fullscreen && c.copyView != nil {
		return c.copyView
	}
	return c.view
}

func (c *frontendController) setFullscreen(enabled bool) {
	if enabled && !c.fullscreen {
		c.copyView = c.view.snapshot()
		c.copyFocused = c.focused
	}
	if !enabled && c.fullscreen && c.copyView != nil {
		// A frozen source anchor remains valid if its owner has not changed.
		if c.copyView.anchor < len(c.copyView.blocks) {
			block := c.copyView.blocks[c.copyView.anchor]
			owner := block.owner
			if owner < len(c.view.lines) && owner < len(c.copyView.lines) && c.view.lines[owner].id == c.copyView.lines[owner].id {
				source := c.copyView.anchorSource
				for n := c.copyView.anchor - 1; n >= 0 && c.copyView.blocks[n].owner == owner; n-- {
					source += len(c.copyView.blocks[n].text)
				}
				for n, b := range c.view.blocks {
					if b.owner == owner {
						c.view.anchor = n
						for c.view.anchor+1 < len(c.view.blocks) && c.view.blocks[c.view.anchor+1].owner == owner && source >= len(c.view.blocks[c.view.anchor].text) {
							source -= len(c.view.blocks[c.view.anchor].text)
							c.view.anchor++
						}
						c.view.anchorSource, c.view.resolveAnchor, c.view.followTail = source, true, c.copyView.followTail
						break
					}
				}
			}
		}
		c.copyView = nil
	}
	c.fullscreen = enabled
	c.dirty = true
	if c.screen != nil {
		if enabled {
			c.screen.DisableMouse()
		} else {
			c.screen.EnableMouse(tcell.MouseButtonEvents)
		}
	}
}

func (c *frontendController) drawFrame() error {
	if c.screen != nil && !c.editing && (!c.fullscreen || c.dirty) {
		if c.modal.menu != nil {
			w, h := c.screen.Size()
			width, height := windowContentSize(w, h, c.modal.window)
			c.modal.menu.reveal(width, height)
		}
		if c.modal.commands != nil {
			w, h := c.screen.Size()
			width, height := windowContentSize(w, h, c.modal.window)
			c.modal.commands.reveal(width, height)
		}
		if c.modal.prompts != nil {
			w, h := c.screen.Size()
			width, height := windowContentSize(w, h, c.modal.window)
			c.modal.prompts.reveal(width, height)
		}
		if c.modal.sessions != nil {
			w, h := c.screen.Size()
			width, height := windowContentSize(w, h, c.modal.window)
			c.modal.sessions.reveal(width, height)
		}
		if c.modal.history != nil && c.modal.window == &c.modal.history.Window {
			w, h := c.screen.Size()
			width, height := windowContentSize(w, h, c.modal.window)
			c.modal.history.reveal(width, height)
		}
		if c.modal.background != nil {
			w, h := c.screen.Size()
			width, height := windowContentSize(w, h, c.modal.window)
			c.modal.background.reveal(width, height)
		}
		if c.modal.question != nil {
			w, h := c.screen.Size()
			width, height := windowContentSize(w, h, c.modal.window)
			c.modal.question.reveal(width, height)
		}
		width, height := c.screen.Size()
		c.sidebar.bounds(width, height, c.fullscreen)
		if !c.commandBusy && c.sidebar.width > 0 {
			c.sidebar.update(c.frontend.Runtime.UsageSnapshot(), c.frontend.Runtime.Jobs.Live(), c.frontend.Runtime.LiveTimers())
		}
		if c.renderer != nil {
			var size tcell.WindowSize
			if c.tty != nil {
				if measured, err := c.tty.WindowSize(); err == nil {
					size = measured
				}
			}
			changed, warning := c.renderer.updateCellDimensions(size)
			if changed {
				c.view.invalidate()
				if c.modal.preview != nil {
					c.modal.preview.cellWidth, c.modal.preview.cellHeight = c.renderer.cellWidth, c.renderer.cellHeight
				}
			}
			if warning != "" {
				c.view.append(line{text: warning, system: true})
			}
			c.g.Begin()
		}
		if c.modal.window != nil && c.modal.window.JobID != "" && !c.commandBusy {
			c.modal.window.Text = c.frontend.Runtime.JobDetail(c.modal.window.JobID, c.modal.window.Detail)
		}
		if c.modal.window != nil && c.modal.window.TimerID != "" && !c.commandBusy {
			if detail, err := c.frontend.Runtime.TimerDetail(c.modal.window.TimerID); err == nil {
				c.modal.window.Text, c.modal.window.Header = detail.Text, detail.Header
				c.modal.window.HeaderFocus = detail.Focus
			} else {
				c.modal.window.Text = "Timer inspection failed: " + err.Error()
				c.modal.window.Header = ""
				c.modal.window.HeaderFocus = ""
			}
		}
		drawFocus := c.focused
		if c.fullscreen {
			drawFocus = c.copyFocused
		}
		previewLimit := max(0, height-4)
		if c.fullscreen {
			previewLimit = 0
		}
		steerCount, steers := c.frontend.Runtime.SteeringPreview(previewLimit)
		indicator := ""
		if c.busy {
			indicator = fmt.Sprintf("%s · %d queued · Esc Esc interrupt · Ctrl+C clear/exit", c.activity.indicator(time.Now(), c.started), len(c.queue)+steerCount)
		} else if c.notificationPaused {
			indicator = "Auto-wake paused · send a prompt or /compact /load /new /model to resume"
		}
		if err := draw(c.screen, c.displayView(), c.sidebar, c.fullscreen, c.modal.preview, c.renderer, drawFocus, c.draft, len(c.attachments), c.queue, steers, indicator, c.modal.window, c.frontend.Runtime.CurrentSelection()); err != nil {
			return err
		}
		if c.renderer != nil {
			if err := c.g.End(); err != nil {
				return err
			}
		}
	}
	c.dirty = false
	return nil
}
