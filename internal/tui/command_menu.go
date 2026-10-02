package tui

import (
	"strings"

	"github.com/gdamore/tcell/v2"
)

// commandMenu selects from the same local catalog used by slash completion.
// Choosing an action places its command in the composer; cancellation keeps the draft.
type commandMenu struct {
	Window   Window
	selected int
	rows     []string
}

func newCommandMenu() *commandMenu {
	m := &commandMenu{}
	m.Window.Title = "Commands"
	m.Window.Hint = "↑↓ select · Enter fills input · Esc closes"
	m.update()
	return m
}

func (m *commandMenu) update() {
	m.rows = m.rows[:0]
	for i, item := range slashCommands {
		m.rows = append(m.rows, menuRow(item.value+" · "+item.description, i == m.selected))
	}
	m.Window.Text = strings.Join(m.rows, "\n")
}

func (m *commandMenu) key(ev *tcell.EventKey, height int) (command string, closed bool) {
	switch ev.Key() {
	case tcell.KeyEscape:
		return "", true
	case tcell.KeyUp:
		m.selected = max(0, m.selected-1)
	case tcell.KeyDown:
		m.selected = min(len(slashCommands)-1, m.selected+1)
	case tcell.KeyHome:
		m.selected = 0
	case tcell.KeyEnd:
		m.selected = len(slashCommands) - 1
	case tcell.KeyPgUp:
		m.selected = max(0, m.selected-max(1, height))
	case tcell.KeyPgDn:
		m.selected = min(len(slashCommands)-1, m.selected+max(1, height))
	case tcell.KeyEnter:
		return slashCommands[m.selected].value, true
	}
	m.update()
	return "", false
}

func (m *commandMenu) reveal(width, height int) {
	row := 0
	for _, line := range m.rows[:m.selected] {
		row += len(wrap(line, max(1, width)))
	}
	if row < m.Window.Scroll {
		m.Window.Scroll = row
	} else if row >= m.Window.Scroll+height {
		m.Window.Scroll = max(0, row-height+1)
	}
}
