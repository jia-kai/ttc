package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"scicode/internal/provider"
	"scicode/internal/session"
)

func TestHeldEditorDrainsEventsAndJoinsOnCancellation(t *testing.T) {
	entered, exited := make(chan string, 1), make(chan struct{})
	u := newQuestionTestUIWithEditor(t, &provider.Script{}, func(ctx context.Context, draft string) (string, error) {
		entered <- draft
		<-ctx.Done()
		close(exited)
		return draft, ctx.Err()
	})
	u.typeText("Held draft")
	u.key(tcell.KeyCtrlX)
	u.typeText("e")
	select {
	case draft := <-entered:
		if draft != "Held draft" {
			t.Fatal(draft)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("editor did not start")
	}
	drained := make(chan struct{})
	go func() {
		// Exceed the event queue capacity while terminal ownership is suspended.
		for range 128 {
			u.runtime.Emit(session.Event{Generation: u.runtime.Generation(), SessionID: u.runtime.Current(), Kind: "status", Text: "While editing"})
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("editor blocked runtime event delivery")
	}
	u.cancel()
	select {
	case err := <-u.done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("frontend did not join canceled editor")
	}
	select {
	case <-exited:
	default:
		t.Fatal("frontend returned before editor exited")
	}
}

func TestReadEditedDraftPreservesFailureAndAcceptsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "draft.md")
	for _, text := range []string{"", "edited\nαβ\n"} {
		os.WriteFile(path, []byte(text), 0600)
		got, err := readEditedDraft(path, "original")
		if err != nil || got != text {
			t.Fatal(got, err)
		}
	}
	os.WriteFile(path, []byte{0xff}, 0600)
	if got, err := readEditedDraft(path, "original"); err == nil || got != "original" {
		t.Fatal(got, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(maxDraftBytes + 1)
	f.Close()
	if got, err := readEditedDraft(path, "original"); err == nil || got != "original" {
		t.Fatal("oversize accepted", err)
	}
}
