package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/history"
	"ttc/internal/render"
)

// sessionMenu is a local metadata snapshot; reload remains an idle runtime command.
// Date headers are not selectable, and wrapping shares the inspector's geometry.
type sessionMenu struct {
	Window       Window
	sessions     []history.Session
	current      string
	selected     int
	rows         []string
	sessionRows  []int
	now          time.Time
	manualScroll bool
}

func newSessionMenu(sessions []history.Session, current string, now time.Time) *sessionMenu {
	m := &sessionMenu{sessions: sessions, current: current, now: now}
	for i, s := range sessions {
		if s.ID == current {
			m.selected = i
			break
		}
	}
	m.Window.Title = "Sessions"
	m.Window.Hint = "↑/↓ select · Enter reload · Esc closes"
	m.update()
	return m
}

func sessionDate(at, now time.Time) string {
	at = at.In(now.Location())
	y, month, d := now.Date()
	today := time.Date(y, month, d, 0, 0, 0, 0, now.Location())
	ay, am, ad := at.Date()
	day := time.Date(ay, am, ad, 0, 0, 0, 0, now.Location())
	if day.Equal(today) {
		return "Today"
	}
	if day.Equal(today.AddDate(0, 0, -1)) {
		return "Yesterday"
	}
	if !day.Before(today.AddDate(0, 0, -6)) && day.Before(today) {
		return day.Weekday().String()
	}
	return day.Format("2006-01-02")
}
func (m *sessionMenu) update() {
	m.rows = nil
	m.sessionRows = nil
	group := ""
	for i, s := range m.sessions {
		label := sessionDate(time.UnixMilli(s.LastActivityMS), m.now)
		if label != group {
			if group != "" {
				m.rows = append(m.rows, "")
			}
			m.rows = append(m.rows, "── "+label+" ──")
			group = label
		}
		name := strings.Join(strings.Fields(render.Clean(s.Name)), " ")
		state := ""
		if s.ID == m.current {
			state = " · current"
		}
		if s.ReadOnly {
			state += " · read-only"
		}
		at := time.UnixMilli(s.LastActivityMS).In(m.now.Location()).Format("15:04")
		m.sessionRows = append(m.sessionRows, len(m.rows))
		m.rows = append(m.rows, menuRow(fmt.Sprintf("%s  %s%s", at, name, state), i == m.selected))
	}
	if len(m.sessions) == 0 {
		m.rows = []string{"No sessions in this workspace"}
	}
	m.Window.Text = strings.Join(m.rows, "\n")
}
func (m *sessionMenu) key(ev *tcell.EventKey, height int) (id string, closed bool) {
	m.manualScroll = false
	switch ev.Key() {
	case tcell.KeyEscape:
		return "", true
	case tcell.KeyUp:
		m.selected = max(0, m.selected-1)
	case tcell.KeyDown:
		m.selected = min(max(0, len(m.sessions)-1), m.selected+1)
	case tcell.KeyHome:
		m.selected = 0
	case tcell.KeyEnd:
		m.selected = max(0, len(m.sessions)-1)
	case tcell.KeyPgUp:
		m.selected = max(0, m.selected-max(1, height/2))
	case tcell.KeyPgDn:
		m.selected = min(max(0, len(m.sessions)-1), m.selected+max(1, height/2))
	case tcell.KeyCtrlU, tcell.KeyCtrlD:
		m.Window.Key(ev, height)
		m.manualScroll = true
	case tcell.KeyEnter:
		if len(m.sessions) > 0 {
			return m.sessions[m.selected].ID, true
		}
	}
	m.update()
	return "", false
}
func (m *sessionMenu) reveal(width, height int) {
	if height <= 0 || m.manualScroll || len(m.sessionRows) == 0 {
		return
	}
	row := 0
	for _, line := range m.rows[:m.sessionRows[m.selected]] {
		row += len(wrap(line, width))
	}
	if row < m.Window.Scroll {
		m.Window.Scroll = row
	} else if row >= m.Window.Scroll+height {
		m.Window.Scroll = row - height + 1
	}
}
func (m *sessionMenu) mouse(ev *tcell.EventMouse, w, h int) {
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
			for j, r := range m.sessionRows {
				if r == i {
					m.selected = j
					m.manualScroll = false
					m.update()
					return
				}
			}
			return
		}
		row = next
	}
}
