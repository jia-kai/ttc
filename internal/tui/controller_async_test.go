package tui

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/session"
	"ttc/internal/skills"
	"ttc/internal/tool"
	"ttc/internal/workspace"
)

// newAsyncTestController uses the real setup/admission/publication boundary but
// leaves event consumption with the test, making interleavings deterministic.
func newAsyncTestController(t *testing.T, provider session.Inference, configure func(*Frontend)) *frontendController {
	t.Helper()
	store, err := history.Open(filepath.Join(t.TempDir(), "history"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	w, err := workspace.Open(t.TempDir(), store)
	if err != nil {
		t.Fatal(err)
	}
	if provider == nil {
		provider = &llm.Script{}
	}
	r, err := session.New(context.Background(), store, w, provider, llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}, "", &skills.Catalog{}, tool.WebSearchConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.AutoName = false
	f := &Frontend{Runtime: r, Plain: true, Input: strings.NewReader(""), Output: io.Discard}
	if configure != nil {
		configure(f)
	}
	c := &frontendController{frontend: f}
	if err := c.setup(context.Background()); err != nil {
		r.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.close(nil); r.Close() })
	for {
		if update := nextControllerUpdate(t, c); update.State != nil {
			break
		}
	}
	c.view = newTranscript()
	return c
}

func nextControllerUpdate(t *testing.T, c *frontendController) session.Update {
	t.Helper()
	select {
	case update := <-c.updates:
		if err := c.handleUpdate(update); err != nil {
			t.Fatal(err)
		}
		if err := c.drawFrame(); err != nil {
			t.Fatal(err)
		}
		return update
	case <-time.After(3 * time.Second):
		t.Fatal("coordinator did not publish the expected update")
		return session.Update{}
	}
}

func waitControllerResult(t *testing.T, c *frontendController, kind session.RequestKind) session.OperationResult {
	t.Helper()
	for {
		if update := nextControllerUpdate(t, c); update.Result != nil && update.Result.Kind == kind {
			return *update.Result
		}
	}
}

func waitControllerState(t *testing.T, c *frontendController, matches func(session.CoordinatorState) bool) session.CoordinatorState {
	t.Helper()
	for {
		if update := nextControllerUpdate(t, c); update.State != nil && matches(*update.State) {
			return *update.State
		}
	}
}
