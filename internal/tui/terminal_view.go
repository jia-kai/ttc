package tui

import (
	"context"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/graphics"
)

// terminalView owns screen initialization and the optional tmux observer.
// Graphics resources are owned separately by the image renderer.
type terminalView struct {
	screen         tcell.Screen
	tty            *terminalTTY
	graphics       *graphics.Kitty
	warning        string
	activation     <-chan struct{}
	stopActivation func()
}

func openTerminalView(ctx context.Context, screen tcell.Screen, g *graphics.Kitty) (*terminalView, error) {
	v := &terminalView{screen: screen, graphics: g}
	if v.screen == nil {
		var err error
		v.screen, v.tty, v.graphics, err = newTerminal(ctx)
		if err != nil {
			return nil, err
		}
	}
	if err := v.screen.Init(); err != nil {
		return nil, err
	}
	if v.tty != nil && v.graphics != nil && v.screen.Colors() < 1<<24 {
		v.graphics = nil
		v.warning = "24-bit color is disabled; Kitty math/images require RGB colors. Remove NO_COLOR or TCELL_TRUECOLOR=disable."
	} else if v.tty != nil && v.graphics == nil {
		v.warning = "Kitty graphics unavailable; math stays as TeX."
		if v.tty.graphicsError != nil {
			v.warning += " " + v.tty.graphicsError.Error()
		}
	}
	v.screen.EnableMouse(tcell.MouseButtonEvents)
	v.screen.EnablePaste()
	v.screen.SetCursorStyle(tcell.CursorStyleBlinkingBar)
	if v.tty != nil && v.graphics != nil && v.graphics.Tmux {
		activation, stop, err := graphics.WatchTmuxActivation(ctx)
		v.activation = activation
		if err != nil {
			v.warning = "Tmux image recovery unavailable: " + err.Error()
		} else {
			v.stopActivation = stop
		}
	}
	if v.graphics != nil && !v.graphics.Tmux {
		v.screen.EnableFocus()
	}
	return v, nil
}

func (v *terminalView) close() {
	if v.stopActivation != nil {
		v.stopActivation()
	}
	v.screen.Fini()
}

func refreshTerminalGraphics(screen tcell.Screen, g *graphics.Kitty, suspended bool) {
	g.Refresh()
	if !suspended {
		screen.Sync()
	}
}
