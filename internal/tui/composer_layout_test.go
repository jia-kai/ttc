package tui

import (
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v3"
)

func viewportRows(v composerViewport) []string {
	rows := make([]string, len(v.rows))
	for i, row := range v.rows {
		rows[i] = row.text
	}
	return rows
}

func TestComposerWrapPreservesSource(t *testing.T) {
	for _, tt := range []struct {
		text  string
		width int
		want  []string
	}{
		{"abcd", 4, []string{"abcd", ""}},
		{"abcd\n", 4, []string{"abcd", ""}},
		{"\n\n", 4, []string{"", "", ""}},
		{"ab界c", 3, []string{"ab", "界c", ""}},
		{"e\u0301👩‍💻x", 3, []string{"e\u0301👩‍💻", "x"}},
		{"a\r\nb\t\x1b", 8, []string{"a�", "b⇥�"}},
	} {
		t.Run(tt.text, func(t *testing.T) {
			c := newComposer(tt.text)
			view := c.viewport(tt.width, 6)
			if got := viewportRows(view); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("rows=%q want=%q", got, tt.want)
			}
			if c.text != tt.text {
				t.Fatal("wrapping changed source", c.text)
			}
			for _, r := range view.rows {
				if ansi.StringWidth(r.text) > tt.width {
					t.Fatal("row overflow", r.text)
				}
			}
		})
	}
}

func TestComposerManualScrollAndReveal(t *testing.T) {
	c := newComposer("0\n1\n2\n3\n4\n5\n6\n7")
	view := c.viewport(10, 6)
	if view.top != 2 || view.cursorY != 5 {
		t.Fatal(view)
	}
	c.scrollTop, c.manualScroll = 0, true
	view = c.viewport(10, 6)
	if view.top != 0 || view.cursorY != -1 || c.cursor != len([]rune(c.text)) {
		t.Fatal(view, c.cursor)
	}
	before := c.text
	for _, k := range []tcell.Key{tcell.KeyCtrlJ, tcell.KeyCtrlK} {
		if c.key(tcell.NewEventKey(k, "", 0)) || c.text != before {
			t.Fatal("scroll shortcut edits text")
		}
	}
	if !c.key(tcell.NewEventKey(tcell.KeyLeft, "", 0)) {
		t.Fatal("movement unhandled")
	}
	if view = c.viewport(10, 6); view.cursorY < 0 || c.manualScroll {
		t.Fatal("cursor not revealed", view)
	}
	c.manualScroll, c.scrollTop = true, 1000
	if view = c.viewport(10, 6); view.top != 2 {
		t.Fatal("scroll not clamped", view)
	}
	c.insert("!")
	if c.manualScroll {
		t.Fatal("insert did not reveal cursor")
	}
}

func TestComposerCursorAtFullHardLine(t *testing.T) {
	for _, text := range []string{"abcd\nx", "ab界\nx"} {
		c := newComposer(text)
		c.cursor = strings.IndexRune(text, '\n')
		c.cursor = utf8.RuneCountInString(text[:c.cursor])
		v := c.viewport(4, 6)
		if v.cursorX != 0 || v.cursorY != 1 {
			t.Fatalf("%q insertion point overlays full line: %+v", text, v)
		}
		c.key(tcell.NewEventKey(tcell.KeyLeft, "", 0))
		if v = c.viewport(4, 6); v.cursorY != 0 {
			t.Fatal("left did not cross full-line boundary", v)
		}
		c.key(tcell.NewEventKey(tcell.KeyRight, "", 0))
		if v = c.viewport(4, 6); v.cursorX != 0 || v.cursorY != 1 {
			t.Fatal("right did not restore full-line insertion point", v)
		}
		c.viewport(5, 6)
		if v = c.viewport(4, 6); v.cursorY != 1 {
			t.Fatal("resize lost full-line insertion point", v)
		}
	}
}

func TestComposerLayoutCacheAndCopies(t *testing.T) {
	c := newComposer(strings.Repeat("word ", 100))
	c.viewport(20, 6)
	cached := c.layout
	c.viewport(20, 6)
	if c.layout != cached {
		t.Fatal("unchanged draft rewrapped")
	}
	copied := c
	copied.insert("new")
	if c.layout != cached || c.text == copied.text {
		t.Fatal("transactional copy mutated original")
	}
	c.viewport(10, 6)
	if c.layout == cached {
		t.Fatal("resize did not rewrap")
	}
	c.key(tcell.NewEventKey(tcell.KeyHome, "", 0))
	if view := c.viewport(10, 6); view.top != 0 || view.cursorY != 0 {
		t.Fatal("cursor not followed on resize", view)
	}
}

func TestInputGeometryBounds(t *testing.T) {
	for _, full := range []bool{false, true} {
		for h := 0; h < 20; h++ {
			for w := 0; w < 20; w++ {
				c := newComposer(strings.Repeat("x\n", 20))
				g := layoutInput(&c, w, h, full, 100)
				if len(g.input.rows) > 6 || g.transcriptHeight < 0 || g.queuedRows < 0 || g.inputY < 0 {
					t.Fatal(w, h, full, g)
				}
				if h > 0 && g.inputY+len(g.input.rows) > h-1 {
					t.Fatal("input overlaps footer", w, h, full, g)
				}
				status := 0
				if !full && h >= 3 {
					status = 1
				}
				if g.transcriptHeight+status+g.queuedRows > g.inputY {
					t.Fatal("rows overlap", w, h, full, g)
				}
				if full && g.queuedRows != 0 {
					t.Fatal("copy mode queue visible")
				}
			}
		}
	}
}

func FuzzComposerLayout(f *testing.F) {
	for _, s := range []string{"", "abc\n", "e\u0301👩‍💻界\t\x1b", "\r\n", strings.Repeat("x", 50)} {
		f.Add(s, uint8(10))
	}
	f.Fuzz(func(t *testing.T, text string, width uint8) {
		if !utf8.ValidString(text) || len(text) > 10000 {
			t.Skip()
		}
		w := int(width%80) + 1
		c := newComposer(text)
		v := c.viewport(w, 6)
		if c.text != text || len(v.rows) > 6 {
			t.Fatal("source/height changed")
		}
		for _, row := range c.layout.rows {
			if !utf8.ValidString(row.text) || ansi.StringWidth(row.text) > w {
				t.Fatalf("unsafe row %q width %d", row.text, w)
			}
			if strings.ContainsFunc(row.text, unicode.IsControl) {
				t.Fatal("control in display")
			}
		}
	})
}
