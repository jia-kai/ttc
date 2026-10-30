package jobs

import (
	"context"
	"errors"
	"io"
	"testing"

	"ttc/internal/lsp"
)

func TestReadGrantsAreReadOnlyAndActorScoped(t *testing.T) {
	ctx := context.Background()
	m := New(ctx, nil)
	defer m.Close()
	release := make(chan struct{})
	id, err := m.StartTask("main", TaskOptions{Kind: "fixture"}, func(ctx context.Context, out, _ io.Writer) (TaskResult, error) {
		io.WriteString(out, "hello\n")
		select {
		case <-release:
		case <-ctx.Done():
		}
		return TaskResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	actor := "main/child_one"
	if err := m.ValidateReadGrants("main", []string{id, "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := m.Read(ctx, actor, id, ReadOptions{Limit: 100}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	m.AddReadGrants(actor, []string{id, id})
	m.mu.Lock()
	m.jobs[id].view.command = "private command"
	m.mu.Unlock()
	if got := m.ListMetadata(actor, true, true); len(got) != 1 || got[0].ID != id || !got[0].ReadOnly || got[0].Command != "" {
		t.Fatal(got)
	}
	if got := m.ListMetadata("main", true, true); len(got) != 1 || got[0].ReadOnly || got[0].Command != "private command" {
		t.Fatal(got)
	}
	if _, err := m.Read(ctx, actor, id, ReadOptions{Limit: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Read(ctx, "main/child_other", id, ReadOptions{Limit: 100}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := m.View(actor, id); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := m.Stop(actor, id); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := m.QueryLSP(ctx, actor, id, lsp.Query{}); err == nil {
		t.Fatal("grant allowed LSP query")
	} else if e, ok := err.(*lsp.Error); !ok || e.Code != "not_found" {
		t.Fatal(err)
	}
	close(release)
	if _, err := m.Wait(ctx, "main", id, nil); err != nil {
		t.Fatal(err)
	}
	result, err := m.Read(ctx, actor, id, ReadOptions{Limit: 100, Grep: "hello"})
	if err != nil || result["output"] != "hello\n" {
		t.Fatal(result, err)
	}
	m.ClearReadGrants(actor)
	if _, err := m.Read(ctx, actor, id, ReadOptions{Limit: 100}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
