package tui

import (
	"context"
	"os"
	"sync"

	"github.com/gdamore/tcell/v3"

	"ttc/internal/graphics"
)

// terminalTTY serializes protocol writes with all tcell writes, including input
// protocol replies. The frontend owns its lifecycle through screen.Fini.
type terminalTTY struct {
	tcell.Tty
	mu            sync.Mutex
	pending       []byte // Input consumed by the capability probe, read once before live input.
	graphicsError error  // Optional graphics detection failure, shown after startup.
}

func (t *terminalTTY) Read(p []byte) (int, error) {
	if len(t.pending) > 0 {
		n := copy(p, t.pending)
		t.pending = t.pending[n:]
		return n, nil
	}
	return t.Tty.Read(p)
}

func (t *terminalTTY) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Tty.Write(p)
}
func newTerminal(ctx context.Context) (tcell.Screen, *terminalTTY, *graphics.Kitty, error) {
	tmux := os.Getenv("TMUX") != ""
	enabled, pending, graphicsError := graphics.Probe(ctx, tmux)
	tty, err := tcell.NewDevTty()
	if err != nil {
		return nil, nil, nil, err
	}
	shared := &terminalTTY{Tty: tty, pending: pending, graphicsError: graphicsError}
	var options []tcell.TerminfoScreenOption
	if enabled && os.Getenv("NO_COLOR") == "" && os.Getenv("TCELL_TRUECOLOR") != "disable" {
		// A successful Kitty query establishes RGB support even when SSH/tmux
		// drops COLORTERM. Unicode placeholders encode image IDs in RGB values.
		// OptColors bypasses tcell's environment detection, so leave explicit
		// color-disable settings to its default path.
		options = append(options, tcell.OptColors(1<<24))
	}
	screen, err := tcell.NewTerminfoScreenFromTty(shared, options...)
	if err != nil {
		tty.Close()
		return nil, nil, nil, err
	}
	var g *graphics.Kitty
	if enabled {
		g = graphics.New(shared, tmux)
	}
	return screen, shared, g, nil
}
