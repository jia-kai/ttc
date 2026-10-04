package tui

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/jobs"
	"ttc/internal/render"
)

type backgroundMenu struct {
	Window   Window
	jobs     []jobs.Snapshot
	selected int
	rows     []string
}

func newBackgroundMenu(list []jobs.Snapshot) *backgroundMenu {
	m := &backgroundMenu{jobs: list}
	m.Window.Title = "Foreground shells"
	m.Window.Hint = "↑↓ select · Enter move to background · Esc closes"
	m.update()
	return m
}

func (m *backgroundMenu) update() {
	rows := []string{}
	for i, j := range m.jobs {
		label := strings.Join(strings.Fields(render.Clean(j.Label)), " ")
		if text := []rune(label); len(text) > 192 {
			label = string(text[:191]) + "…"
		}
		rows = append(rows, menuRow(fmt.Sprintf("%s · %s", j.ID, label), i == m.selected))
	}
	m.rows = rows
	m.Window.Text = strings.Join(rows, "\n")
}

func (m *backgroundMenu) key(ev *tcell.EventKey) (id string, closed bool) {
	switch ev.Key() {
	case tcell.KeyEscape:
		return "", true
	case tcell.KeyUp:
		m.selected = max(0, m.selected-1)
	case tcell.KeyDown:
		m.selected = min(len(m.jobs)-1, m.selected+1)
	case tcell.KeyEnter:
		if len(m.jobs) > 0 {
			return m.jobs[m.selected].ID, true
		}
	}
	m.update()
	return "", false
}

func (m *backgroundMenu) reveal(width, height int) {
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

func (m *backgroundMenu) mouse(ev *tcell.EventMouse, w, h int) {
	if ev.Buttons()&tcell.WheelUp != 0 {
		m.selected = max(0, m.selected-3)
		m.update()
		return
	}
	if ev.Buttons()&tcell.WheelDown != 0 {
		m.selected = min(len(m.jobs)-1, m.selected+3)
		m.update()
		return
	}
	if ev.Buttons()&tcell.Button1 == 0 {
		return
	}
	x, y := ev.Position()
	left, top, width, height := windowBounds(w, h)
	if x <= left || x >= left+width-1 || y <= top || y >= top+height-1 {
		return
	}
	target := m.Window.Scroll + y - top - 1
	row := 0
	for i, line := range m.rows {
		next := row + len(wrap(line, max(1, width-2)))
		if target >= row && target < next {
			m.selected = i
			m.update()
			return
		}
		row = next
	}
}
