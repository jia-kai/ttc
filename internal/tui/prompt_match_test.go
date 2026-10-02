package tui

import (
	"reflect"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v2"
)

func TestPromptSearchAllTermsAnyOrderWhitespaceAndSubstrings(t *testing.T) {
	entries := []string{"Beta then ALPHABET", "alpha only", "beta only", "α \nİSTANBUL beta", "alpha and beta"}
	for _, test := range []struct {
		query string
		want  []int
	}{
		{"alpha beta", []int{4, 0}},
		{" BeTa\tALpHa\n ", []int{4, 0}},
		{"alp pha", []int{4, 1, 0}},
		{"α ist", []int{3}},
		{"beta beta", []int{4, 3, 2, 0}},
		{" \t\n", []int{4, 3, 2, 1, 0}},
		{"alpha missing", nil},
	} {
		m := newPromptSearch(entries)
		m.query.set(test.query)
		m.filter()
		if !reflect.DeepEqual(m.matches, test.want) && !(len(m.matches) == 0 && len(test.want) == 0) {
			t.Fatal(test.query, m.matches, test.want)
		}
		if len(m.matches) > 0 {
			text, closed := m.key(tcell.NewEventKey(tcell.KeyEnter, 0, 0), 10)
			if !closed || text != entries[test.want[0]] {
				t.Fatal("recall altered prompt", text)
			}
		}
	}
}

func TestPromptHighlightUnicodeOverlapAndEveryOccurrence(t *testing.T) {
	for _, test := range []struct {
		text  string
		terms []string
		want  string
	}{
		{"Alpha BETA alpha", []string{"alpha", "beta"}, "\x1b[1;4mAlpha\x1b[0m \x1b[1;4mBETA\x1b[0m \x1b[1;4malpha\x1b[0m"},
		{"İSTANBUL 界α", []string{"ist", "α"}, "\x1b[1;4mİST\x1b[0mANBUL 界\x1b[1;4mα\x1b[0m"},
		{"banana", []string{"ana", "nan"}, "b\x1b[1;4manana\x1b[0m"},
		{"unchanged", nil, "unchanged"},
	} {
		got := highlightPrompt(test.text, test.terms)
		if got != test.want || ansi.Strip(got) != test.text || !utf8.ValidString(got) {
			t.Fatal(got, test.want)
		}
	}
}

func TestPromptHighlightDrawAndWrappedSelection(t *testing.T) {
	m := newPromptSearch([]string{"older Alpha beta", "newer alpha and BETA"})
	m.query.set("beta alpha")
	m.filter()
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(30, 24)
	drawWindow(s, &m.Window)
	underlined := 0
	for y := 0; y < 24; y++ {
		for x := 0; x < 30; x++ {
			_, _, style, _ := s.GetContent(x, y)
			_, _, attrs := style.Decompose()
			if attrs&tcell.AttrUnderline != 0 {
				underlined++
			}
		}
	}
	if underlined != 18 {
		t.Fatal("matched text did not render highlighted", underlined)
	}
	left, top, width, height := windowBounds(30, 24)
	headers := len(m.Window.HeaderLines(width-2, height-3))
	row := len(wrapStyled(m.rows[0], width-2))
	m.mouse(tcell.NewEventMouse(left+1, top+headers+row+1, tcell.Button1, 0), 30, 24)
	if m.selected != 1 {
		t.Fatal("highlight changed mouse hit testing", m.selected)
	}
	for _, row := range m.Window.Lines(8, 50) {
		if ansi.StringWidth(row) > 8 {
			t.Fatal("styles counted as display cells", row)
		}
	}
}
