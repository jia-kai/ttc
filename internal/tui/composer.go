package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v2"
)

// composer owns a draft, a rune-offset cursor and the last killed text. Editing
// is by rune; display clipping preserves graphemes. A new draft keeps the kill
// buffer, while history recall places the cursor at the recalled draft's end.
type composer struct {
	text       string
	cursor     int
	killed     string
	completion *completionMenu // UI-owned dropdown; independent of editable text.
	pasting    bool
}

func newComposer(text string) composer {
	var c composer
	c.set(text)
	return c
}

func (c *composer) set(text string) {
	c.text, c.cursor = text, utf8.RuneCountInString(text)
}

func (c *composer) insert(text string) {
	runes := []rune(c.text)
	c.text = string(runes[:c.cursor]) + text + string(runes[c.cursor:])
	c.cursor += utf8.RuneCountInString(text)
}

func (c *composer) erase(start, end int, kill bool) {
	if start == end {
		return
	}
	runes := []rune(c.text)
	if kill {
		c.killed = string(runes[start:end])
	}
	c.text, c.cursor = string(runes[:start])+string(runes[end:]), start
}

func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func wordBefore(runes []rune, at int) int {
	for at > 0 && !wordRune(runes[at-1]) {
		at--
	}
	for at > 0 && wordRune(runes[at-1]) {
		at--
	}
	return at
}

func wordAfter(runes []rune, at int) int {
	for at < len(runes) && !wordRune(runes[at]) {
		at++
	}
	for at < len(runes) && wordRune(runes[at]) {
		at++
	}
	return at
}

// key handles composer editing only; scrolling, submission, menus and recall
// belong to the frontend. Line keys use logical newline boundaries.
func (c *composer) key(ev *tcell.EventKey) bool {
	runes := []rune(c.text)
	if ev.Modifiers()&tcell.ModAlt != 0 {
		switch ev.Key() {
		case tcell.KeyRune:
			switch ev.Rune() {
			case 'b':
				c.cursor = wordBefore(runes, c.cursor)
			case 'f':
				c.cursor = wordAfter(runes, c.cursor)
			case 'd':
				c.erase(c.cursor, wordAfter(runes, c.cursor), true)
			default:
				return false
			}
		case tcell.KeyBackspace, tcell.KeyBackspace2:
			c.erase(wordBefore(runes, c.cursor), c.cursor, true)
		default:
			return false
		}
		return true
	}
	switch ev.Key() {
	case tcell.KeyLeft, tcell.KeyCtrlB:
		c.cursor = max(0, c.cursor-1)
	case tcell.KeyRight, tcell.KeyCtrlF:
		c.cursor = min(len(runes), c.cursor+1)
	case tcell.KeyHome, tcell.KeyCtrlA:
		for c.cursor > 0 && runes[c.cursor-1] != '\n' {
			c.cursor--
		}
	case tcell.KeyEnd, tcell.KeyCtrlE:
		for c.cursor < len(runes) && runes[c.cursor] != '\n' {
			c.cursor++
		}
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		c.erase(max(0, c.cursor-1), c.cursor, false)
	case tcell.KeyDelete:
		c.erase(c.cursor, min(len(runes), c.cursor+1), false)
	case tcell.KeyCtrlW:
		start := c.cursor
		for start > 0 && unicode.IsSpace(runes[start-1]) {
			start--
		}
		for start > 0 && !unicode.IsSpace(runes[start-1]) {
			start--
		}
		c.erase(start, c.cursor, true)
	case tcell.KeyCtrlK:
		end := c.cursor
		for end < len(runes) && runes[end] != '\n' {
			end++
		}
		if end == c.cursor && end < len(runes) {
			end++ // Kill the newline when already at the end of a logical line.
		}
		c.erase(c.cursor, end, true)
	case tcell.KeyCtrlY:
		c.insert(c.killed)
	case tcell.KeyEnter:
		if ev.Modifiers()&tcell.ModShift == 0 {
			return false
		}
		c.insert("\n")
	case tcell.KeyRune:
		c.insert(string(ev.Rune()))
	default:
		return false
	}
	return true
}

// pasteKey inserts decoded bracketed-paste text without interpreting it as a
// command, menu key or submission. Other control keys are ignored.
func (c *composer) pasteKey(ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyRune:
		c.insert(string(ev.Rune()))
	case tcell.KeyEnter, tcell.KeyCtrlJ:
		c.insert("\n")
	case tcell.KeyTab:
		c.insert("\t")
	}
}

// viewport returns one safe input row and its cursor column, in terminal cells.
// The view follows the cursor; newline/tab markers preserve the submitted text.
func (c composer) viewport(width int) (string, int) {
	if width <= 0 {
		return "", 0
	}
	visible := func(text string) string {
		return strings.Map(func(r rune) rune {
			switch r {
			case '\n':
				return '↵'
			case '\t':
				return '⇥'
			}
			if unicode.IsControl(r) {
				return '�'
			}
			return r
		}, text)
	}
	text := visible(c.text)
	x := ansi.StringWidth(visible(string([]rune(c.text)[:c.cursor])))
	for x >= width && text != "" {
		cluster, cells := ansi.FirstGraphemeCluster(text, ansi.GraphemeWidth)
		text, x = text[len(cluster):], x-cells
	}
	return ansi.Truncate(text, width, ""), max(0, x)
}
