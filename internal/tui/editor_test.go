package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gdamore/tcell/v3"
	"golang.org/x/sys/unix"
	"ttc/internal/llm"
	"ttc/internal/session"
)

func TestHeldEditorDrainsEventsAndJoinsOnCancellation(t *testing.T) {
	entered, exited := make(chan string, 1), make(chan struct{})
	u := newQuestionTestUIWithEditor(t, &llm.Script{}, func(ctx context.Context, draft string) (string, error) {
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

func TestReadEditedDraftTrimsTrailingWhitespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "draft.md")
	for _, tt := range []struct {
		name, text, want string
	}{
		{name: "empty"},
		{name: "unchanged", text: "edited\nαβ", want: "edited\nαβ"},
		{name: "final newline", text: "edited\nαβ\n", want: "edited\nαβ"},
		{name: "trailing spaces", text: "edited   ", want: "edited"},
		{name: "mixed suffix", text: "edited \t\r\n\n \t", want: "edited"},
		{name: "unicode whitespace", text: "αβ\u00a0\u2003\n", want: "αβ"},
		{name: "whitespace only", text: " \t\r\n\n"},
		{name: "preserve leading and internal whitespace", text: "\n  first  \n\n\tsecond\n", want: "\n  first  \n\n\tsecond"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tt.text), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := readEditedDraft(path, "original")
			if err != nil || got != tt.want {
				t.Fatalf("readEditedDraft() = %q, %v; want %q, nil", got, err, tt.want)
			}
		})
	}
}

func TestReadEditedDraftPreservesFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "draft.md")
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

func TestReadEditedDraftRejectsSubstitutedFIFOAndSymlink(t *testing.T) {
	for _, kind := range []string{"fifo", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "input.md")
			if kind == "fifo" {
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				target := filepath.Join(root, "other.md")
				if err := os.WriteFile(target, []byte("unrelated contents"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			type result struct {
				text string
				err  error
			}
			done := make(chan result, 1)
			go func() {
				text, err := readEditedDraft(path, "preserve draft")
				done <- result{text, err}
			}()
			select {
			case got := <-done:
				if got.err == nil || got.text != "preserve draft" {
					t.Fatalf("unsafe editor replacement changed draft: %q %v", got.text, got.err)
				}
			case <-time.After(time.Second):
				if kind == "fifo" {
					// Release a regression to blocking open before reporting failure.
					f, err := os.OpenFile(path, os.O_WRONLY|unix.O_NONBLOCK, 0)
					if err == nil {
						f.Close()
						<-done
					}
				}
				t.Fatal("replacement validation blocked")
			}
		})
	}
}
