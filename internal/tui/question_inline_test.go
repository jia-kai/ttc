package tui

import (
	"bytes"
	"fmt"
	"image"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/assets"
	contextbuild "ttc/internal/context"
	"ttc/internal/graphics"
	"ttc/internal/workspace"
)

func TestQuestionInlineReviewRevealsSubmitButton(t *testing.T) {
	s := newTestScreen(t)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(110, 42) // The demo's terminal geometry.
	d := newQuestionDialog(questionFixture())
	for range d.form.Questions {
		if answers, dismissed := d.key(tcell.NewEventKey(tcell.KeyEnter, "", 0), 11); answers != nil || dismissed {
			t.Fatal("selecting an answer submitted or dismissed the form")
		}
	}
	paintInlineQuestion(t, s, d)
	text := screenText(s)
	for _, want := range []string{"[Submit]", "> [ Submit answers ]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("review target %q is not visible: %s", want, text)
		}
	}
	// Help below the focused button need not be visible; submission depends on
	// the visible control rather than the viewport including that help text.
	answers, dismissed := d.key(tcell.NewEventKey(tcell.KeyEnter, "", 0), 11)
	if dismissed || len(answers) != len(d.form.Questions) {
		t.Fatalf("explicit Submit did not return all answers: %+v, dismissed=%v", answers, dismissed)
	}
}

func TestQuestionInlineGeometryBounds(t *testing.T) {
	for height := 0; height <= 80; height++ {
		for width := 0; width <= 3; width++ {
			for _, fullscreen := range []bool{false, true} {
				g := layoutQuestionInput(width, height, fullscreen, 1000)
				if g.bodyWidth < 0 || g.bodyHeight < 0 || g.transcriptHeight < 0 || g.queuedRows < 0 || g.headerY < g.transcriptHeight || g.bodyY < g.headerY || g.bodyY+g.bodyHeight > max(0, height-1) {
					t.Fatalf("size %dx%d fullscreen=%v: invalid geometry %+v", width, height, fullscreen, g)
				}
				if height >= 3 && g.transcriptHeight < height/2 {
					t.Fatalf("size %dx%d: transcript lost useful half: %+v", width, height, g)
				}
				if g.bodyY+g.bodyHeight-g.headerY > min(12, height/2) {
					t.Fatalf("size %dx%d: question exceeds bound: %+v", width, height, g)
				}
			}
		}
	}
}

func TestQuestionInlinePreservesTranscriptSidebarAndHidesDraft(t *testing.T) {
	s := newTestScreen(t)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(140, 40)
	view := newTranscript()
	for i := range 100 {
		view.append(line{text: fmt.Sprintf("context line %03d", i), id: int64(i + 1)})
	}
	bar := newSidebar()
	bar.workspace = workspace.GitInfo{Cwd: "/research/project", Repo: "/research", Branch: "main"}
	d := questionRenderDraft("answer", 3)
	draft := newComposer("private normal draft")
	queue := []contextbuild.Input{{Text: "later work"}}
	paint := func() questionInputGeometry {
		t.Helper()
		if err := draw(s, drawState{
			view: view, sidebar: bar, focused: -1, draft: draft, attached: 3,
			queue: queue, steers: []string{"next steering"}, indicator: "Working", window: &d.Window, question: d,
		}); err != nil {
			t.Fatal(err)
		}
		w, h := s.Size()
		return layoutQuestionInput(bar.bounds(w, h, false), h, false, len(queue)+1)
	}
	g := paint()
	frame := screenText(s)
	for _, want := range []string{"context line 099", "[1] | Submit", "Text: answer", "Steer · next steering", "Queued · later work", "Ctrl+U/D context", "main"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q:\n%s", want, frame)
		}
	}
	for _, hidden := range []string{"private normal draft", "attachments", "┌Questions"} {
		if strings.Contains(frame, hidden) {
			t.Fatalf("question exposed %q:\n%s", hidden, frame)
		}
	}
	if bar.width == 0 || g.queuedRows != 2 {
		t.Fatalf("sidebar or queue lost: width=%d geometry=%+v", bar.width, g)
	}
	before := view.firstLine
	view.scroll(-g.transcriptHeight/2, g.transcriptHeight)
	paint()
	if view.firstLine >= before || !strings.Contains(screenText(s), "Text: answer") {
		t.Fatal("transcript did not scroll independently of question")
	}
}

func TestQuestionInlineTinyResizeAndFixedHeader(t *testing.T) {
	s := newTestScreen(t)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	d := questionRenderDraft(strings.Repeat("line\n", 50), 0)
	for _, size := range [][2]int{{80, 24}, {20, 10}, {1, 1}, {1, 2}, {2, 3}, {6, 5}, {80, 24}} {
		s.SetSize(size[0], size[1])
		g := paintInlineQuestion(t, s, d)
		if s.cursorVisible && (s.cursorX < 0 || s.cursorX >= g.bodyWidth || s.cursorY < g.bodyY || s.cursorY >= g.bodyY+g.bodyHeight) {
			t.Fatalf("size %v: cursor outside body: %d,%d geometry=%+v", size, s.cursorX, s.cursorY, g)
		}
		if g.bodyY > g.headerY && g.bodyWidth >= len("[1]") {
			if got := sidebarScreenText(s, 0, g.headerY, g.bodyWidth); !strings.Contains(got, "[1]") {
				t.Fatalf("size %v: fixed active tab missing: %q", size, got)
			}
		}
	}
}

func TestQuestionInlineSidebarOverlayStaysAboveForm(t *testing.T) {
	s := newTestScreen(t)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(80, 24)
	bar := newSidebar()
	bar.overlay = true
	d := questionRenderDraft(strings.Repeat("x", 60), 60)
	if err := draw(s, drawState{
		view: newTranscript(), sidebar: bar, focused: -1, draft: newComposer(""), window: &d.Window, question: d,
	}); err != nil {
		t.Fatal(err)
	}
	g := layoutQuestionInput(80, 24, false, 0)
	y := g.bodyY + d.Window.body.cursor.y - d.Window.Scroll
	if got := sidebarScreenText(s, bar.left, y, bar.width); strings.Contains(got, "x") || s.cursorVisible {
		t.Fatalf("form drew above sidebar overlay: row=%q cursorVisible=%v", got, s.cursorVisible)
	}
}

func TestQuestionInlineKeepsVisibleTranscriptKittyGraphics(t *testing.T) {
	var wire bytes.Buffer
	g := graphics.New(&wire, false)
	key := assets.Key("mathjax-hires-v1", "test", "x", "16", fmt.Sprint(assets.MathRasterScale))
	r := &imageRenderer{graphics: g, backend: "test", cellWidth: 8, cellHeight: 16, ready: map[string]renderReply{key: {pixels: image.NewRGBA(image.Rect(0, 0, 120, 36))}}}
	view := transcriptOf([]line{{text: "Visible context $x$", speaker: "assistant", markdown: true}})
	view.layout = r.layout
	s := newTestScreen(t)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(80, 24)
	d := questionRenderDraft("answer", 2)
	g.Begin()
	if err := draw(s, drawState{
		view: view, sidebar: newSidebar(), renderer: r, focused: -1, draft: newComposer("hidden"), window: &d.Window, question: d,
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.End(); err != nil {
		t.Fatal(err)
	}
	geometry := layoutQuestionInput(80, 24, false, 0)
	count := 0
	for y := range geometry.transcriptHeight {
		for x := range 80 {
			cluster, style, _ := s.Get(x, y)
			if strings.HasPrefix(cluster, "\U0010eeee") {
				if len([]rune(cluster)) != 4 || !style.GetForeground().Valid() {
					t.Fatalf("lost Kitty placeholder identity at %d,%d: %q", x, y, cluster)
				}
				count++
			}
		}
	}
	if count == 0 || !strings.Contains(wire.String(), "a=T") || !strings.Contains(screenText(s), "Text: answer") {
		t.Fatalf("visible transcript graphic or question missing: placeholders=%d wire=%q", count, wire.String())
	}
}
