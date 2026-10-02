package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v2"
	"scicode/internal/assets"
	"scicode/internal/graphics"
	"scicode/internal/provider"
)

func TestMarkdownWindowRefreshesPendingMathAndRequestsOnlyVisibleAssets(t *testing.T) {
	var wire bytes.Buffer
	g := graphics.New(&wire, false)
	r := &imageRenderer{
		graphics: g, ctx: context.Background(), cellWidth: 8, cellHeight: 16,
		ready: map[string]renderReply{}, pending: map[string]context.CancelFunc{},
		tasks: make(chan renderTask, 32),
	}
	w := &Window{Title: "Assistant", Markdown: true, Text: "before $x^2$ after\n\n" + strings.Repeat("Other paragraph.\n\n", 80) + "$y^2$"}
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(60, 16)
	view := newTranscript()
	frame := func() {
		t.Helper()
		g.Begin()
		if err := draw(s, view, newSidebar(), false, nil, r, -1, newComposer(""), 0, nil, nil, "", w, provider.Selection{}); err != nil {
			t.Fatal(err)
		}
		if err := g.End(); err != nil {
			t.Fatal(err)
		}
	}
	frame() // Initialization is asynchronous; the first frame is readable TeX.
	if len(r.tasks) != 0 {
		t.Fatal("requested formulas before backend initialization")
	}
	r.accept(renderReply{backend: "fixture"})
	frame()
	if len(r.tasks) != 1 {
		t.Fatal("requested formulas outside popup viewport", len(r.tasks))
	}
	task := <-r.tasks
	if task.tex != "x^2" {
		t.Fatal("wrong visible formula", task.tex)
	}
	if !r.accept(renderReply{key: task.key, pixels: image.NewRGBA(image.Rect(0, 0, 72, 36)), math: true}) {
		t.Fatal("discarded visible reply")
	}
	frame()
	count := 0
	for y := range 16 {
		for x := range 60 {
			main, marks, _, _ := s.GetContent(x, y)
			if main == '\U0010eeee' {
				count++
				if len(marks) != 3 {
					t.Fatal("popup lost image coordinates", marks)
				}
			}
		}
	}
	if count != 3 || !strings.Contains(wire.String(), "a=T") {
		t.Fatal("popup did not replace pending TeX with image", count)
	}
	for _, text := range w.Lines(54, 10) {
		if strings.Contains(ansi.Strip(text), "x^2") {
			t.Fatal("stale popup layout survived reply")
		}
	}
	// New cell measurements invalidate the popup even if its text/width match.
	old := w.cachedRevision
	r.updateCellDimensions(tcell.WindowSize{Width: 60, Height: 16, PixelWidth: 600, PixelHeight: 320})
	frame()
	if w.cachedRevision == old || w.cachedRevision != r.revision {
		t.Fatal("popup kept stale pixel geometry")
	}
}

func TestMathFailuresAreLabeledAndDoNotSpin(t *testing.T) {
	var wire bytes.Buffer
	r := &imageRenderer{graphics: graphics.New(&wire, false), ctx: context.Background(), backend: "fixture", cellWidth: 8, cellHeight: 16, ready: map[string]renderReply{}, pending: map[string]context.CancelFunc{}, tasks: make(chan renderTask, 32)}
	key := assets.Key("mathjax-hires-v1", "fixture", "x^2", "16", fmt.Sprint(assets.MathRasterScale))
	for _, backend := range []bool{false, true} {
		if backend {
			r.backendError = errors.New("node is required")
			r.ready = map[string]renderReply{}
		} else {
			r.ready[key] = renderReply{err: errors.New("unsupported formula")}
		}
		rows := r.layout(line{text: "before $x^2$ after", markdown: true}, 80)
		var visible []line
		var text strings.Builder
		for _, row := range rows {
			text.WriteString(ansi.Strip(row.text))
			visible = append(visible, line{text: row.text, assets: row.assets})
		}
		if !strings.Contains(text.String(), "[math unavailable] x^2") {
			t.Fatal("render failure silently returned raw TeX", text.String())
		}
		if err := r.ensure(visible); err != nil || len(r.tasks) != 0 {
			t.Fatal("permanent failure was retried in draw loop", err)
		}
	}
}

func TestInlineMathPaintsKittyIdentityAndCoordinates(t *testing.T) {
	for _, source := range []string{
		"before $x$ after", "before \\(x\\) after", "**before $x$ after**",
		"*before $x$ after*", "> before $x$ after", "- before $x$ after",
		"| Value | Meaning |\n| --- | --- |\n| $x$ | variable |",
		"before $x$ then $x$ after", "before $x$ after with `code`",
	} {
		for _, width := range []int{20, 80} {
			t.Run(fmt.Sprintf("%s/%d", source, width), func(t *testing.T) {
				var wire bytes.Buffer
				g := graphics.New(&wire, false)
				g.Begin()
				key := assets.Key("mathjax-hires-v1", "test", "x", "16", fmt.Sprint(assets.MathRasterScale))
				pixels := image.NewRGBA(image.Rect(0, 0, 120, 36))
				for y := range 36 {
					for x := range 120 {
						pixels.SetRGBA(x, y, color.RGBA{255, 255, 255, 255})
					}
				}
				r := &imageRenderer{graphics: g, backend: "test", cellWidth: 8, cellHeight: 16, ready: map[string]renderReply{key: {pixels: pixels}}, pending: map[string]context.CancelFunc{}}
				v := transcriptOf([]line{{text: source, speaker: "assistant", markdown: true}})
				v.layout = r.layout
				s := tcell.NewSimulationScreen("UTF-8")
				if err := s.Init(); err != nil {
					t.Fatal(err)
				}
				defer s.Fini()
				s.SetSize(width, 30)
				if err := draw(s, v, newSidebar(), false, nil, r, -1, newComposer(""), 0, nil, nil, "", nil, provider.Selection{}); err != nil {
					t.Fatal(err)
				}
				// Compare against a clean standalone grid, independently of prose
				// Markdown styles and inline replacement/slicing.
				grid, err := g.Rows(key, 5, 1)
				if err != nil {
					t.Fatal(err)
				}
				ref := tcell.NewSimulationScreen("UTF-8")
				if err := ref.Init(); err != nil {
					t.Fatal(err)
				}
				defer ref.Fini()
				ref.SetSize(5, 1)
				putStyled(ref, 0, 0, 5, grid[0], tcell.StyleDefault)
				_, _, expectedStyle, _ := ref.GetContent(0, 0)
				expectedFG, _, _ := expectedStyle.Decompose()
				count := 0
				for y := range 27 {
					for x := range width {
						main, marks, style, _ := s.GetContent(x, y)
						if main != '\U0010eeee' {
							continue
						}
						fg, _, _ := style.Decompose()
						if fg != expectedFG || len(marks) != 3 {
							t.Fatalf("lost Kitty image identity at %d,%d: fg=%v want=%v marks=%v", x, y, fg, expectedFG, marks)
						}
						count++
					}
				}
				if count == 0 || !strings.Contains(wire.String(), "a=T") {
					t.Fatal("inline math was not painted/uploaded", v.visible, wire.String())
				}
			})
		}
	}
}
