package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/render"
)

// promptSearch searches a frozen, bounded prompt snapshot. Folded text is built
// once on opening; editing the query does no database reads or message rendering.
type promptSearch struct {
	Window          Window
	query           composer
	entries, folded []string
	terms           []string // Folded AND terms, longest first for early rejection.
	matches         []int    // Original prompt indices, newest first.
	selected, start int
	rows            []string
}

func newPromptSearch(entries []string) *promptSearch {
	m := &promptSearch{entries: append([]string(nil), entries...), query: newComposer("")}
	m.folded = make([]string, len(entries))
	for i, text := range entries {
		m.folded[i] = strings.ToLower(text)
	}
	m.Window.Title = "Prompt history search"
	m.Window.Styled = true
	m.Window.Hint = "All words match · ↑↓ select · Ctrl-R older · Enter fills input · Esc cancels"
	m.filter()
	return m
}

func (m *promptSearch) filter() {
	m.terms = searchTerms(m.query.text)
	m.matches = m.matches[:0]
	for i := len(m.entries) - 1; i >= 0; i-- {
		if matchesSearchTerms(m.folded[i], m.terms) {
			m.matches = append(m.matches, i)
		}
	}
	m.selected, m.start, m.Window.Scroll = 0, 0, 0
	m.update()
}

func (m *promptSearch) update() {
	m.selected = max(0, min(m.selected, len(m.matches)-1))
	if m.selected < m.start || m.selected >= m.start+historyWindowRows {
		m.start = max(0, m.selected-historyWindowRows/2)
		m.Window.Scroll = 0
	}
	query := []rune(m.query.text)
	m.Window.Header = "Search: " + string(query[:m.query.cursor]) + "▏" + string(query[m.query.cursor:]) + fmt.Sprintf("\nMatches %d · %d recent prompts · newest first", len(m.matches), len(m.entries))
	m.rows = m.rows[:0]
	for i := m.start; i < min(len(m.matches), m.start+historyWindowRows); i++ {
		text := m.entries[m.matches[i]]
		// Limit before sanitizing; huge prompts stay cheap to browse and exact to recall.
		if len(text) > 512 {
			end := 512
			for !utf8.RuneStart(text[end]) {
				end--
			}
			text = text[:end] + "…"
		}
		text = strings.ReplaceAll(render.Clean(text), "\n", " ↵ ")
		text = highlightPrompt(text, m.terms)
		m.rows = append(m.rows, menuRow(text, i == m.selected))
	}
	m.Window.Text = strings.Join(m.rows, "\n")
	if len(m.matches) == 0 {
		m.Window.Text = "No matching prompts"
	}
}

func (m *promptSearch) key(ev *tcell.EventKey, height int) (text string, closed bool) {
	before := m.query.text
	if m.query.pasting {
		m.query.pasteKey(ev)
	} else {
		switch ev.Key() {
		case tcell.KeyEscape:
			return "", true
		case tcell.KeyEnter:
			if len(m.matches) > 0 {
				return m.entries[m.matches[m.selected]], true
			}
			return "", false
		case tcell.KeyUp:
			m.selected--
		case tcell.KeyDown, tcell.KeyCtrlR:
			m.selected++
		case tcell.KeyPgUp:
			m.selected -= max(1, height/2)
		case tcell.KeyPgDn:
			m.selected += max(1, height/2)
		case tcell.KeyCtrlU:
			m.query.set("")
		default:
			m.query.key(ev)
		}
	}
	// Bound pasted search text too; this affects only the query, never recalled prompts.
	if utf8.RuneCountInString(m.query.text) > 256 {
		m.query.set(string([]rune(m.query.text)[:256]))
	}
	if m.query.text != before {
		m.filter()
	} else {
		m.update()
	}
	return "", false
}

func (m *promptSearch) reveal(width, height int) {
	if height <= 0 || len(m.rows) == 0 {
		return
	}
	row := 0
	for _, line := range m.rows[:m.selected-m.start] {
		row += len(wrapStyled(line, max(1, width)))
	}
	if row < m.Window.Scroll {
		m.Window.Scroll = row
	} else if row >= m.Window.Scroll+height {
		m.Window.Scroll = row - height + 1
	}
}

func (m *promptSearch) mouse(ev *tcell.EventMouse, w, h int) {
	if ev.Buttons()&tcell.WheelUp != 0 {
		m.selected -= 3
		m.update()
		return
	}
	if ev.Buttons()&tcell.WheelDown != 0 {
		m.selected += 3
		m.update()
		return
	}
	if ev.Buttons()&tcell.Button1 == 0 {
		return
	}
	x, y := ev.Position()
	left, top, width, height := windowBounds(w, h)
	inner := max(1, width-2)
	headers := len(m.Window.HeaderLines(inner, max(0, height-3)))
	if x <= left || x >= left+width-1 || y <= top+headers || y >= top+height-1 {
		return
	}
	target, row := m.Window.Scroll+y-top-headers-1, 0
	for i, line := range m.rows {
		next := row + len(wrapStyled(line, inner))
		if target >= row && target < next {
			m.selected = m.start + i
			m.update()
			return
		}
		row = next
	}
}

// highlightPrompt operates only on bounded, sanitized display text. Mapping the
// folded byte offsets back to original rune boundaries handles changing UTF-8
// widths (for example İ→i). Marking a union merges overlapping/repeated matches.
func highlightPrompt(text string, terms []string) string {
	if len(terms) == 0 {
		return text
	}
	folded := strings.ToLower(text)
	marked := make([]bool, len(folded))
	for _, term := range terms {
		for start := 0; start < len(folded); {
			at := strings.Index(folded[start:], term)
			if at < 0 {
				break
			}
			at += start
			for i := at; i < at+len(term); i++ {
				marked[i] = true
			}
			_, size := utf8.DecodeRuneInString(folded[at:])
			start = at + size
		}
	}
	var out strings.Builder
	offset, active := 0, false
	for _, r := range text {
		_, size := utf8.DecodeRuneInString(folded[offset:])
		match := marked[offset]
		if match != active {
			if match {
				out.WriteString("\x1b[1;4m")
			} else {
				out.WriteString("\x1b[0m")
			}
			active = match
		}
		out.WriteRune(r)
		offset += size
	}
	if active {
		out.WriteString("\x1b[0m")
	}
	return out.String()
}
