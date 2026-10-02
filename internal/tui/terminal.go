package tui

import (
	"context"
	"github.com/gdamore/tcell/v2"
	"os"
	"scicode/internal/graphics"
	"sync"
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
	info, err := tcell.LookupTerminfo(os.Getenv("TERM"))
	if err != nil {
		tty.Close()
		return nil, nil, nil, err
	}
	if enabled {
		// A successful Kitty query establishes RGB support even when SSH/tmux
		// drops COLORTERM. Unicode placeholders encode image IDs in RGB values.
		// Copy the shared terminfo entry so other screens keep their own settings.
		copy := *info
		copy.SetFgRGB = "\x1b[38;2;%p1%d;%p2%d;%p3%dm"
		copy.SetBgRGB = "\x1b[48;2;%p1%d;%p2%d;%p3%dm"
		copy.SetFgBgRGB = "\x1b[38;2;%p1%d;%p2%d;%p3%d;48;2;%p4%d;%p5%d;%p6%dm"
		info = &copy
	}
	screen, err := tcell.NewTerminfoScreenFromTtyTerminfo(shared, info)
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
