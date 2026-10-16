package tui

import (
	"context"
	"io"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/catalog"
	"ttc/internal/graphics"
	"ttc/internal/llm"
	"ttc/internal/session"
)

// Frontend wires a runtime into either a full terminal view or redirected plain I/O.
type Frontend struct {
	Screen   tcell.Screen    // Optional injected terminal screen for deterministic UI tests.
	Graphics *graphics.Kitty // Optional protocol sink for deterministic graphics tests.
	Runtime  *session.Runtime
	Events   <-chan session.Event
	Input    io.Reader
	Output   io.Writer
	Plain    bool
	// Login authorizes and returns a fresh catalog. The owner must cancel/join
	// old discovery and discard stale deliveries before returning, even on error.
	// Nil disables interactive authorization; errors leave the picker unavailable.
	Login     func(context.Context) ([]llm.ModelSpec, error)
	EditInput func(context.Context, string) (string, error) // Optional editor override for embedding/tests.
	Models    []llm.ModelSpec                               // Initial catalog; owned by the event loop during Run.
	// ModelUpdates optionally delivers externally refreshed catalogs. Nil disables
	// delivery; closure disables further receives. Open pickers retain their snapshot,
	// but submitted choices resolve against the latest catalog and may return an error.
	ModelUpdates <-chan catalog.Update
}

// Run keeps input available while a model or foreground tool runs.
// In the TUI, Enter steers active main work and Alt+Enter queues FIFO.
// Plain-mode messages queue while busy.
func (f *Frontend) Run(ctx context.Context) (runErr error) {
	c := &frontendController{frontend: f}
	defer func() { runErr = c.close(runErr) }()
	if err := c.setup(ctx); err != nil {
		return err
	}
	for {
		if stop, err := c.prepare(); stop || err != nil {
			return err
		}
		if err := c.drawFrame(); err != nil {
			return err
		}
		next, stop, err := c.receive()
		if stop || err != nil {
			return err
		}
		if next.ready {
			if c.submit(next) {
				return nil
			}
		}
	}
}
