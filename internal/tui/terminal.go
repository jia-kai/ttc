package tui

import (
	"github.com/gdamore/tcell/v2"
	"os"
	"scicode/internal/graphics"
	"sync"
)

// terminalTTY serializes protocol writes with all tcell writes, including input
// protocol replies. The frontend owns its lifecycle through screen.Fini.
type terminalTTY struct {
	tcell.Tty
	mu      sync.Mutex
	pending []byte // Input consumed by the capability probe, read once before live input.
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
func newTerminal() (tcell.Screen, *terminalTTY, *graphics.Kitty, error) {
	tmux := os.Getenv("TMUX") != ""
	enabled, pending := graphics.Probe(tmux)
	tty, err := tcell.NewDevTty()
	if err != nil {
		return nil, nil, nil, err
	}
	shared := &terminalTTY{Tty: tty, pending: pending}
	screen, err := tcell.NewTerminfoScreenFromTty(shared)
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
