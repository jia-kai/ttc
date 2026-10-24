package tui

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v3"
	"ttc/internal/graphics"
)

func documentFixture() string {
	var text strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&text, "document-row-%03d  \n", i)
	}
	return text.String()
}

func waitDocumentFrame(t *testing.T, frames <-chan string, needle string) string {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case frame := <-frames:
			if strings.Contains(frame, needle) {
				return frame
			}
		case <-deadline.C:
			t.Fatalf("document frame missing %q", needle)
		}
	}
}

func TestDocumentFullWidthScrollResize(t *testing.T) {
	screen := &observedScreen{Screen: newTestScreen(t), frames: make(chan string, 64)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- (&Document{Title: "fixture.md", Text: documentFixture(), Screen: screen}).Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("document did not stop")
		}
	}()
	initial := waitDocumentFrame(t, screen.frames, "lines 1–24/")
	if !strings.Contains(initial, "fixture.md") || strings.Contains(initial, "> ") || strings.Contains(initial, "Workspace") {
		t.Fatalf("unexpected session UI: %s", initial)
	}
	screen.EventQ() <- tcell.NewEventKey(tcell.KeyCtrlD, "", 0)
	waitDocumentFrame(t, screen.frames, "lines 13–36/")
	screen.EventQ() <- tcell.NewEventKey(tcell.KeyCtrlU, "", 0)
	waitDocumentFrame(t, screen.frames, "lines 1–24/")
	screen.SetSize(100, 15)
	screen.EventQ() <- tcell.NewEventResize(100, 15)
	resized := waitDocumentFrame(t, screen.frames, "lines 1–14/")
	if rows := strings.Split(strings.TrimSuffix(resized, "\n"), "\n"); len(rows) != 15 || len(rows[0]) != 100 {
		t.Fatalf("resize not applied: %q", resized)
	}
	screen.EventQ() <- tcell.NewEventMouse(1, 1, tcell.WheelDown, 0)
	waitDocumentFrame(t, screen.frames, "lines 4–17/")
}

func TestDocumentExitAndCancellation(t *testing.T) {
	cases := []struct {
		name   string
		key    *tcell.EventKey
		cancel bool
	}{
		{name: "q", key: tcell.NewEventKey(tcell.KeyRune, "q", 0)},
		{name: "escape", key: tcell.NewEventKey(tcell.KeyEscape, "", 0)},
		{name: "ctrl-c", key: tcell.NewEventKey(tcell.KeyCtrlC, "", 0)},
		{name: "cancel", cancel: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			screen := &observedScreen{Screen: newTestScreen(t), frames: make(chan string, 64)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- (&Document{Title: "empty.md", Screen: screen}).Run(ctx) }()
			waitDocumentFrame(t, screen.frames, "empty.md")
			if tc.cancel {
				cancel()
			} else {
				screen.EventQ() <- tc.key
			}
			select {
			case err := <-done:
				if tc.cancel && !errors.Is(err, context.Canceled) || !tc.cancel && err != nil {
					t.Fatalf("Run: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Run did not exit")
			}
		})
	}
}

func TestDocumentGridUsesEntireViewport(t *testing.T) {
	screen := newTestScreen(t)
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	window := &Window{Markdown: true, Text: documentFixture()}
	warning := ""
	if err := drawDocument(&terminalView{screen: screen}, window, "fixture.md", &warning, nil); err != nil {
		t.Fatal(err)
	}
	if window.cachedWidth != 80 {
		t.Fatalf("render width %d; want full 80 columns", window.cachedWidth)
	}
	if rows := window.Lines(80, 24); len(rows) != 24 {
		t.Fatalf("body height %d; want 24", len(rows))
	}
	screen.SetSize(1, 1)
	if err := drawDocument(&terminalView{screen: screen}, window, "fixture.md", &warning, nil); err != nil {
		t.Fatal(err)
	}
	if documentHeight(1) != 1 || window.cachedWidth != 1 {
		t.Fatal("single-cell terminal lost content")
	}
}

func TestDocumentWindowKeepsWholeMarkdownStructure(t *testing.T) {
	const distant = 20 << 10
	t.Run("reference link", func(t *testing.T) {
		window := &Window{Markdown: true, Text: "[distant reference][target]\n\n" + strings.Repeat("padding paragraph\n\n", distant/19) + "\n[target]: https://example.test/resolved\n"}
		rows := window.Lines(80, 4)
		if text := ansi.Strip(strings.Join(rows, "\n")); !strings.Contains(text, "https://example.test/resolved") || strings.Contains(text, "[target]") {
			t.Fatalf("distant link definition was not resolved: %q", text)
		}
	})
	t.Run("ordered list", func(t *testing.T) {
		window := &Window{Markdown: true, Text: "1. first-item\n" + strings.Repeat("   continuing the same list item\n", distant/30) + "2. last-item\n"}
		window.Scroll = 1 << 30
		text := ansi.Strip(strings.Join(window.Lines(80, 4), "\n"))
		if !strings.Contains(text, "2. last-item") {
			t.Fatalf("list numbering lost across a long item: %q", text)
		}
	})
	t.Run("table", func(t *testing.T) {
		window := &Window{Markdown: true, Text: "| heading | value |\n| --- | --- |\n" + strings.Repeat("| table row | content |\n", distant/24) + "| last-row | final-cell |\n"}
		window.Scroll = 1 << 30
		for _, row := range window.Lines(80, 4) {
			if strings.Contains(row, "last-row") {
				if plain := ansi.Strip(row); !strings.Contains(plain, "│") || strings.Contains(plain, "|") {
					t.Fatalf("table tail was parsed outside its table: %q", plain)
				}
				return
			}
		}
		t.Fatal("table tail missing")
	})
}

func TestDocumentWindowReusesParsedMarkdown(t *testing.T) {
	for _, withGraphics := range []bool{false, true} {
		t.Run(fmt.Sprintf("graphics=%v", withGraphics), func(t *testing.T) {
			window := &Window{Text: "before $x$ after", Markdown: true}
			if withGraphics {
				g := graphics.New(io.Discard, false)
				defer g.Close()
				window.renderer = &imageRenderer{
					graphics: g, cellWidth: 8, cellHeight: 16,
					ready: map[string]renderReply{}, visible: map[string]bool{},
				}
			}
			window.Lines(80, 10)
			parsed := window.parsedDocument
			if parsed == nil {
				t.Fatal("document was not parsed")
			}
			window.Lines(40, 10)
			if window.parsedDocument != parsed {
				t.Fatal("resize reparsed unchanged Markdown")
			}
			if withGraphics {
				r := window.renderer
				r.revision++
				window.Lines(40, 10)
				if window.parsedDocument != parsed {
					t.Fatal("raster revision reparsed unchanged Markdown")
				}
				key := ""
				for _, row := range window.assets {
					for _, asset := range row.assets {
						if asset.tex == "x" {
							key = asset.key
						}
					}
				}
				if key == "" {
					t.Fatal("formula asset missing")
				}
				r.visible[key] = true
				if !r.accept(renderReply{key: key, math: true, pixels: image.NewRGBA(image.Rect(0, 0, 32, 32))}) {
					t.Fatal("visible raster reply was not accepted")
				}
				rows := window.Lines(40, 10)
				if window.parsedDocument != parsed || !strings.ContainsRune(strings.Join(rows, "\n"), '\U0010eeee') {
					t.Fatal("raster result did not update existing parsed document", rows)
				}
			}
			window.Text = "changed $y$ source"
			window.Lines(40, 10)
			if window.parsedDocument == parsed {
				t.Fatal("changed source reused stale Markdown AST")
			}
		})
	}
}

func TestDocumentGraphicsExitJoinsWorker(t *testing.T) {
	isolatedImageFrontend(t)
	screen := &observedScreen{Screen: newTestScreen(t), frames: make(chan string, 64)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- (&Document{Title: "graphics.md", Text: "$x$", Screen: screen, Graphics: graphics.New(io.Discard, false)}).Run(ctx)
	}()
	waitDocumentFrame(t, screen.frames, "100%")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("graphics worker did not join")
	}
}

type documentFinalizingScreen struct {
	*observedScreen
	finalized bool
}

func (s *documentFinalizingScreen) Fini() {
	s.observedScreen.Fini()
	s.finalized = true
}

func TestDocumentTerminalInputErrorRestoresScreen(t *testing.T) {
	isolatedImageFrontend(t)
	screen := &documentFinalizingScreen{observedScreen: &observedScreen{Screen: newTestScreen(t), frames: make(chan string, 64)}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- (&Document{Title: "error.md", Screen: screen, Graphics: graphics.New(io.Discard, false)}).Run(ctx)
	}()
	waitDocumentFrame(t, screen.frames, "100%")
	screen.EventQ() <- tcell.NewEventError(io.ErrUnexpectedEOF)
	select {
	case err := <-done:
		if !errors.Is(err, io.ErrUnexpectedEOF) || !strings.Contains(err.Error(), "terminal input") {
			t.Fatalf("terminal failure not propagated: %v", err)
		}
		if !screen.finalized {
			t.Fatal("terminal failure did not finalize screen")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("terminal failure did not stop and join document workers")
	}
}

func TestDocumentGraphicsRefreshAndViewport(t *testing.T) {
	isolatedImageFrontend(t)
	screen := newTestScreen(t)
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	g := graphics.New(io.Discard, false)
	renderer, err := newImageRenderer(context.Background(), g, "")
	if err != nil {
		t.Fatal(err)
	}
	defer renderer.close()
	// Pre-cached rows isolate viewport placement from optional math binaries.
	window := &Window{Markdown: true, renderer: renderer, Text: "fixture", cachedText: "fixture", cachedWidth: 80, cachedMode: 1, cachedColors: true}
	window.cachedLines = make([]string, 30)
	window.assets = make([]displayRow, 30)
	for i := range window.assets {
		key := fmt.Sprintf("image-%d", i)
		grid, err := g.Rows(key, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		window.cachedLines[i] = grid[0]
		window.assets[i] = displayRow{text: grid[0], assets: []placedImage{{key: key, columns: 1, rows: 1}}}
		renderer.ready[key] = renderReply{pixels: image.NewRGBA(image.Rect(0, 0, 1, 1))}
	}
	renderer.imageBytes = 30 * 4
	t.Setenv("NO_COLOR", "")
	warning := ""
	terminal := &terminalView{screen: screen, graphics: g}
	if err := drawDocument(terminal, window, "fixture.md", &warning, nil); err != nil {
		t.Fatal(err)
	}
	if len(renderer.visible) != 24 || renderer.visible["image-24"] {
		t.Fatalf("assets outside viewport requested: %v", renderer.visible)
	}
	refreshTerminalGraphics(screen, g, false)
	if err := drawDocument(terminal, window, "fixture.md", &warning, nil); err != nil {
		t.Fatal(err)
	}
}
