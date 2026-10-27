package tui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/session"
)

func questionRenderDraft(text string, cursor int) *questionDialog {
	d := newQuestionDialog(session.QuestionForm{ID: "render", Questions: []session.Question{{ID: "answer", Prompt: "Prompt"}}})
	d.answers[0].custom = newComposer(text)
	d.answers[0].custom.cursor = cursor
	d.update()
	return d
}

func paintInlineQuestion(t *testing.T, s *testScreen, d *questionDialog) questionInputGeometry {
	t.Helper()
	w, h := s.Size()
	if err := draw(s, drawState{
		view: newTranscript(), sidebar: newSidebar(), focused: -1, draft: newComposer("hidden draft"), attached: 2,
		window: &d.Window, question: d,
	}); err != nil {
		t.Fatal(err)
	}
	return layoutQuestionInput(w, h, false, 0)
}

func TestQuestionRenderComposerGeometry(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		width, at  int
		rows       []string
		x, y       int // Relative to the first input row, including its label.
	}{
		{"middle", "abcd", 12, 2, []string{"Text: abcd"}, 8, 0},
		{"wide and combining", "界e\u0301x", 12, 3, []string{"Text: 界e\u0301x"}, 9, 0},
		{"emoji cluster", "👩‍💻x", 12, 3, []string{"Text: 👩‍💻x"}, 8, 0},
		{"wrapped middle", "abcdefghi", 10, 5, []string{"Text: abcd", "      efgh", "      i"}, 7, 1},
		{"exact width end", "abcd", 10, 4, []string{"Text: abcd", "      "}, 6, 1},
		{"newline", "ab\ncd", 10, 3, []string{"Text: ab", "      cd"}, 6, 1},
		{"full row before newline", "abcd\nx", 10, 4, []string{"Text: abcd", "      x"}, 6, 1},
		{"trailing newline", "ab\n", 10, 3, []string{"Text: ab", "      "}, 6, 1},
		{"empty", "", 10, 0, []string{"Text: "}, 6, 0},
		{"label width", "abcdefg", 6, 6, []string{"Text:", "abcdef", "g"}, 0, 2},
		{"narrow label", "abcd", 4, 4, []string{"Text", ":", "abcd", ""}, 0, 3},
		{"one cell wide glyph", "界", 1, 1, []string{"T", "e", "x", "t", ":", "�", ""}, 0, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := questionRenderDraft(tc.text, tc.at)
			d.Window.Lines(tc.width, 100)
			body := d.Window.body
			start := 0
			for _, row := range d.rows[:d.inputRow] {
				start += len(wrap(row, tc.width))
			}
			if got := body.lines[start : start+len(tc.rows)]; !reflect.DeepEqual(got, tc.rows) {
				t.Fatalf("input rows = %#v, want %#v", got, tc.rows)
			}
			want := cellPosition{tc.x, start + tc.y}
			if body.cursor == nil || *body.cursor != want || body.focusRow != want.y {
				t.Fatalf("cursor = %v, focus = %d; want %v", body.cursor, body.focusRow, want)
			}
			if strings.Contains(strings.Join(body.lines, "\n"), "▏") {
				t.Fatal("body contains a synthetic cursor cell")
			}
		})
	}
}

func TestQuestionRenderNativeCursorAndMovement(t *testing.T) {
	s := newTestScreen(t)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(50, 24)
	d := questionRenderDraft("abcd", 2)
	paint := func(wantX int) {
		t.Helper()
		geometry := paintInlineQuestion(t, s, d)
		y := geometry.bodyY + d.inputRow - d.Window.Scroll
		if !s.cursorVisible || s.cursorX != wantX || s.cursorY != y {
			t.Fatalf("native cursor = (%d,%d,%v), want (%d,%d,true)", s.cursorX, s.cursorY, s.cursorVisible, wantX, y)
		}
		if frame := screenText(s); !strings.Contains(frame, "Text: abcd") || strings.Contains(frame, "▏") {
			t.Fatalf("text is not contiguous beneath the cursor:\n%s", frame)
		}
	}
	paint(8)
	before := d.answers[0].custom.text
	for _, key := range []struct {
		key tcell.Key
		x   int
	}{{tcell.KeyLeft, 7}, {tcell.KeyHome, 6}, {tcell.KeyEnd, 10}, {tcell.KeyLeft, 9}} {
		d.key(tcell.NewEventKey(key.key, "", 0), 10)
		if d.answers[0].custom.text != before {
			t.Fatal("cursor movement changed the text")
		}
		paint(key.x)
	}
}

func TestQuestionRenderResizeAndNarrowWindow(t *testing.T) {
	s := newTestScreen(t)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	d := questionRenderDraft("abcdefghij", 10)
	for _, size := range []struct{ width, height int }{{32, 22}, {16, 22}, {8, 22}, {5, 22}, {32, 22}} {
		s.SetSize(size.width, size.height)
		geometry := paintInlineQuestion(t, s, d)
		width, height := geometry.bodyWidth, geometry.bodyHeight
		body := d.Window.body
		if body.cursor == nil || body.cursor.x < 0 || body.cursor.x >= width || body.cursor.y < d.Window.Scroll || body.cursor.y >= d.Window.Scroll+height {
			t.Fatalf("size %v: cursor %v outside body viewport width=%d height=%d scroll=%d", size, body.cursor, width, height, d.Window.Scroll)
		}
		want := cellPosition{body.cursor.x, geometry.bodyY + body.cursor.y - d.Window.Scroll}
		if !s.cursorVisible || (cellPosition{s.cursorX, s.cursorY}) != want {
			t.Fatalf("size %v: screen cursor=(%d,%d,%v), want %v", size, s.cursorX, s.cursorY, s.cursorVisible, want)
		}
		if d.answers[0].custom.text != "abcdefghij" || d.answers[0].custom.cursor != 10 {
			t.Fatal("resize changed the draft")
		}
	}
}

func TestQuestionRenderManualScrollAndInactiveCursor(t *testing.T) {
	s := newTestScreen(t)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(40, 12)
	d := questionRenderDraft(strings.Repeat("line\n", 24)+"end", 0)
	d.form.Questions[0].Prompt = strings.Repeat("prompt\n", 20)
	d.update()
	geometry := layoutQuestionInput(40, 12, false, 0)
	width, height := geometry.bodyWidth, geometry.bodyHeight
	for _, scroll := range []int{0, 1 << 20} {
		d.Window.Scroll, d.manualScroll = scroll, true
		d.Window.Lines(width, height)
		clamped := d.Window.Scroll
		d.reveal(width, height)
		paintInlineQuestion(t, s, d)
		if d.Window.Scroll != clamped || s.cursorVisible {
			t.Fatalf("manual scroll moved or exposed offscreen cursor: scroll=%d want=%d visible=%v", d.Window.Scroll, clamped, s.cursorVisible)
		}
	}
	d.key(tcell.NewEventKey(tcell.KeyRight, "", 0), height)
	d.reveal(width, height)
	paintInlineQuestion(t, s, d)
	if d.manualScroll || !s.cursorVisible {
		t.Fatal("editing did not restore cursor-following")
	}
	for _, key := range []tcell.Key{tcell.KeyEscape, tcell.KeyTab} {
		d.key(tcell.NewEventKey(key, "", 0), height)
		d.reveal(width, height)
		paintInlineQuestion(t, s, d)
		if d.Window.body.cursor != nil || s.cursorVisible {
			t.Fatalf("cursor visible outside text entry after %v", key)
		}
	}
	if d.tab != len(d.answers) {
		t.Fatal("fixture did not reach Submit")
	}
}

func TestQuestionRenderEditsMatchComposer(t *testing.T) {
	legacy := func(key tcell.Key) *tcell.EventKey { return tcell.NewEventKey(key, "", 0) }
	runeKey := func(text string, mod tcell.ModMask) *tcell.EventKey {
		return tcell.NewEventKey(tcell.KeyRune, text, mod)
	}
	for _, tc := range []struct {
		name string
		keys []*tcell.EventKey
	}{
		{"runes and deletion", []*tcell.EventKey{runeKey("a界e\u0301", 0), legacy(tcell.KeyLeft), legacy(tcell.KeyBackspace), legacy(tcell.KeyHome), legacy(tcell.KeyDelete), legacy(tcell.KeyEnd), runeKey("!", 0)}},
		{"logical lines", []*tcell.EventKey{runeKey("first line", 0), tcell.NewEventKey(tcell.KeyEnter, "", tcell.ModShift), runeKey("second", 0), legacy(tcell.KeyHome), legacy(tcell.KeyRight), legacy(tcell.KeyEnd), legacy(tcell.KeyBackspace)}},
		{"kill yank words", []*tcell.EventKey{runeKey("one two 界", 0), legacy(tcell.KeyCtrlW), legacy(tcell.KeyCtrlY), runeKey("b", tcell.ModAlt), runeKey("d", tcell.ModAlt), legacy(tcell.KeyCtrlY), legacy(tcell.KeyBackspace), tcell.NewEventKey(tcell.KeyBackspace, "", tcell.ModAlt), legacy(tcell.KeyCtrlY), runeKey("f", tcell.ModAlt)}},
		{"control reports", []*tcell.EventKey{runeKey("abc def", 0), runeKey("a", tcell.ModCtrl), runeKey("f", tcell.ModCtrl), runeKey("b", tcell.ModCtrl), runeKey("e", tcell.ModCtrl), runeKey("w", tcell.ModCtrl), runeKey("y", tcell.ModCtrl), runeKey("h", tcell.ModCtrl)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, want := questionRenderDraft("", 0), newComposer("")
			for i, key := range tc.keys {
				if !want.key(key) {
					t.Fatalf("step %d is not a shared composer key", i)
				}
				answers, dismissed := d.key(key, 8)
				got := d.answers[0].custom
				if answers != nil || dismissed || !d.textEntry() || got.text != want.text || got.cursor != want.cursor || got.killed != want.killed {
					t.Fatalf("step %d: question=(%q,%d,%q), composer=(%q,%d,%q), answers=%v dismissed=%v", i, got.text, got.cursor, got.killed, want.text, want.cursor, want.killed, answers, dismissed)
				}
			}
		})
	}
}

func TestQuestionRenderDrawUsesQuestionCursorInsteadOfMainInput(t *testing.T) {
	s := newTestScreen(t)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(60, 24)
	d := questionRenderDraft("answer", 2)
	main := newComposer("main draft")
	for _, window := range []*Window{nil, &d.Window} {
		var question *questionDialog
		if window != nil {
			question = d
		}
		if err := draw(s, drawState{
			view: newTranscript(), sidebar: newSidebar(), focused: -1, draft: main, window: window, question: question,
		}); err != nil {
			t.Fatal(err)
		}
		if !s.cursorVisible {
			t.Fatal("active input cursor is hidden")
		}
		if window == nil {
			geometry := layoutInput(&main, 60, 24, false, 0)
			if s.cursorX != 12 || s.cursorY != geometry.inputY {
				t.Fatalf("main cursor=(%d,%d)", s.cursorX, s.cursorY)
			}
		} else {
			geometry := layoutQuestionInput(60, 24, false, 0)
			if s.cursorX != 8 || s.cursorY != geometry.bodyY+d.inputRow-d.Window.Scroll {
				t.Fatalf("question cursor=(%d,%d)", s.cursorX, s.cursorY)
			}
			if strings.Contains(screenText(s), "main draft") {
				t.Fatal("question exposed the main draft")
			}
		}
	}
}
