package tui

import (
	"reflect"
	"testing"
	"unicode/utf8"

	"github.com/gdamore/tcell/v3"
)

type composerEditingCase struct {
	name, text string
	cursor     int
	keys       []*tcell.EventKey
	want       string
	at         int
}

func composerEditingCases() []composerEditingCase {
	key := func(k tcell.Key) *tcell.EventKey { return tcell.NewEventKey(k, "", 0) }
	alt := func(r rune) *tcell.EventKey { return tcell.NewEventKey(tcell.KeyRune, string(r), tcell.ModAlt) }
	return []composerEditingCase{
		{"word movement", "one two/three", 13, []*tcell.EventKey{alt('b'), alt('b'), alt('f')}, "one two/three", 7},
		{"whitespace word deletion", "one two/three  ", 15, []*tcell.EventKey{key(tcell.KeyCtrlW)}, "one ", 4},
		{"middle word deletion", "one two/three", 7, []*tcell.EventKey{key(tcell.KeyCtrlW)}, "one /three", 4},
		{"forward kill and yank", "one two/three", 3, []*tcell.EventKey{alt('d'), key(tcell.KeyCtrlY)}, "one two/three", 7},
		{"alt backspace", "one two/three", 13, []*tcell.EventKey{tcell.NewEventKey(tcell.KeyBackspace, "", tcell.ModAlt)}, "one two/", 8},
		{"insert in middle", "λ界x", 1, []*tcell.EventKey{tcell.NewEventKey(tcell.KeyRune, "β", 0)}, "λβ界x", 2},
		{"delete wide rune", "λ界x", 1, []*tcell.EventKey{key(tcell.KeyDelete)}, "λx", 1},
		{"backspace", "λ界x", 2, []*tcell.EventKey{key(tcell.KeyBackspace)}, "λx", 1},
		{"logical line home", "one\ntwo\nthree", 6, []*tcell.EventKey{key(tcell.KeyCtrlA)}, "one\ntwo\nthree", 4},
		{"logical line end", "one\ntwo\nthree", 4, []*tcell.EventKey{key(tcell.KeyEnd)}, "one\ntwo\nthree", 7},
		{"unicode word", "αβ 界字 tail", 5, []*tcell.EventKey{key(tcell.KeyCtrlW)}, "αβ  tail", 3},
		{"character movement", "αβ", 0, []*tcell.EventKey{key(tcell.KeyCtrlF), key(tcell.KeyRight), key(tcell.KeyCtrlB), key(tcell.KeyLeft), key(tcell.KeyLeft)}, "αβ", 0},
		{"empty edges", "", 0, []*tcell.EventKey{key(tcell.KeyDelete), key(tcell.KeyBackspace), key(tcell.KeyCtrlW), alt('b'), alt('f')}, "", 0},
		{"shift enter", "ab", 1, []*tcell.EventKey{tcell.NewEventKey(tcell.KeyEnter, "", tcell.ModShift)}, "a\nb", 2},
	}
}

func TestComposerEditing(t *testing.T) {
	for _, tt := range composerEditingCases() {
		t.Run(tt.name, func(t *testing.T) {
			c := newComposer(tt.text)
			c.cursor = tt.cursor
			for _, event := range tt.keys {
				if !c.key(event) {
					t.Fatalf("unhandled editing key: %v", event)
				}
			}
			if c.text != tt.want || c.cursor != tt.at || !utf8.ValidString(c.text) {
				t.Fatalf("%q at %d; want %q at %d", c.text, c.cursor, tt.want, tt.at)
			}
		})
	}
}

func TestComposerKeepsKillBufferAndLeavesReservedKeys(t *testing.T) {
	c := newComposer("old word")
	c.key(tcell.NewEventKey(tcell.KeyCtrlW, "", 0))
	c.set("new ")
	c.key(tcell.NewEventKey(tcell.KeyCtrlY, "", 0))
	if c.text != "new word" {
		t.Fatal(c.text)
	}
	for _, key := range []tcell.Key{tcell.KeyCtrlC, tcell.KeyCtrlD, tcell.KeyCtrlU, tcell.KeyCtrlJ, tcell.KeyCtrlK, tcell.KeyUp, tcell.KeyDown, tcell.KeyEnter, tcell.KeyTab} {
		if c.key(tcell.NewEventKey(key, "", 0)) {
			t.Fatalf("composer consumed reserved key %v", key)
		}
	}
}

func TestComposerViewport(t *testing.T) {
	for _, tt := range []struct {
		text                string
		want                []string
		cursor, width, x, y int
	}{
		{"abcdef", []string{"abcd", "ef"}, 6, 4, 2, 1},
		{"abcdef", []string{"abcd", "ef"}, 2, 4, 2, 0},
		{"α界abc", []string{"α界a", "bc"}, 5, 4, 2, 1},
		{"é界x", []string{"é界x"}, 2, 10, 1, 0},
		{"a\n\tb\x1b", []string{"a", "⇥b�"}, 5, 10, 3, 1},
		{"界", []string{"�", ""}, 1, 1, 0, 1},
		{"x", []string{"x", ""}, 1, 0, 0, -1},
	} {
		c := newComposer(tt.text)
		c.cursor = tt.cursor
		view := c.viewport(tt.width, 6)
		var rows []string
		for _, row := range view.rows {
			rows = append(rows, row.text)
		}
		if !reflect.DeepEqual(rows, tt.want) || view.cursorX != tt.x || view.cursorY != tt.y {
			t.Fatalf("viewport for %q: %q at (%d,%d), want %q at (%d,%d)", tt.text, rows, view.cursorX, view.cursorY, tt.want, tt.x, tt.y)
		}
	}
}
