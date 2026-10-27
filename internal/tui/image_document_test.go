package tui

import (
	"context"
	"errors"
	"image"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/graphics"
)

func TestStandaloneImageExit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   *tcell.EventKey
		mouse bool
	}{
		{name: "escape", key: tcell.NewEventKey(tcell.KeyEscape, "", 0)},
		{name: "q", key: tcell.NewEventKey(tcell.KeyRune, "q", 0)},
		{name: "ctrl-c", key: tcell.NewEventKey(tcell.KeyCtrlC, "", 0)},
		{name: "mouse", mouse: true},
		{name: "cancel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A standalone image must work without an asset/MathJax cache or
			// external rendering programs; an invalid cache root catches worker
			// initialization before any subprocess could run.
			cacheRoot := filepath.Join(t.TempDir(), "blocked-cache")
			if err := os.WriteFile(cacheRoot, nil, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_CACHE_HOME", cacheRoot)
			t.Setenv("PATH", t.TempDir())
			screen := &observedScreen{Screen: newTestScreen(t), frames: make(chan string, 64)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- (&Image{Path: "fixture.png", Pixels: image.NewRGBA(image.Rect(0, 0, 20, 10)), Screen: screen, Graphics: graphics.New(io.Discard, false)}).Run(ctx)
			}()
			waitDocumentFrame(t, screen.frames, "fixture.png")
			// Preview controls remain available before exiting the standalone view.
			screen.EventQ() <- tcell.NewEventKey(tcell.KeyRune, "+", 0)
			waitDocumentFrame(t, screen.frames, "fixture.png")
			if tc.mouse {
				w, h := screen.Size()
				preview := newImagePreview(imageSource{}, image.NewRGBA(image.Rect(0, 0, 20, 10)), false, 8, 16)
				preview.geometry(w, h)
				screen.EventQ() <- tcell.NewEventMouse(preview.left, preview.top+preview.height+1, tcell.Button1, 0)
			} else if tc.key != nil {
				screen.EventQ() <- tc.key
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if tc.name == "cancel" && !errors.Is(err, context.Canceled) || tc.name != "cancel" && err != nil {
					t.Fatalf("Run: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("standalone image did not exit")
			}
		})
	}
}

func TestStandaloneImageRequiresPixelsAndGraphics(t *testing.T) {
	if err := (&Image{}).Run(context.Background()); err == nil {
		t.Fatal("accepted missing pixels")
	}
	screen := &documentFinalizingScreen{observedScreen: &observedScreen{Screen: newTestScreen(t), frames: make(chan string, 64)}}
	err := (&Image{Pixels: image.NewRGBA(image.Rect(0, 0, 2, 2)), Screen: screen}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Kitty") || !screen.finalized {
		t.Fatalf("missing graphics: %v, screen finalized: %v", err, screen.finalized)
	}
}
