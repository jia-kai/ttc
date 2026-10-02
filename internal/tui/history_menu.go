package tui

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"scicode/internal/history"
	"scicode/internal/render"
)

const historyWindowRows = 128

// historyMenu keeps a metadata snapshot and publishes only a small nearby slice
// to the shared window. Selection therefore does not rewrap the whole history.
type historyMenu struct {
	Window   Window
	tree     history.BranchTree
	nodes    []history.BranchNode
	depths   []int
	indices  map[int64]int
	children map[int64][]int64
	targets  map[int64]int64 // Human input ID to its pre-input restoration checkpoint.
	selected int
	start    int
	rows     []string
}

func newHistoryMenu(tree history.BranchTree) *historyMenu {
	m := &historyMenu{tree: tree, indices: map[int64]int{}, children: map[int64][]int64{}, targets: map[int64]int64{}}
	byID := map[int64]history.BranchNode{}
	original := map[int64]history.BranchNode{}
	nearest := map[int64]int64{}
	for _, node := range tree.Nodes {
		original[node.ID] = node
		nearest[node.ID] = nearest[node.Parent]
		if node.UserInput {
			parent := original[node.Parent]
			m.targets[node.ID] = node.Parent
			node.Parent = nearest[node.Parent]
			node.Restorable, node.Reason = parent.Restorable, parent.Reason
			byID[node.ID] = node
			m.children[node.Parent] = append(m.children[node.Parent], node.ID)
			nearest[node.ID] = node.ID
		}
	}
	type visit struct {
		id    int64
		depth int
	}
	var stack []visit
	for i := len(m.children[0]) - 1; i >= 0; i-- {
		stack = append(stack, visit{id: m.children[0][i]})
	}
	for len(stack) > 0 {
		v := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		node, found := byID[v.id]
		if !found {
			continue
		}
		m.indices[v.id] = len(m.nodes)
		m.nodes = append(m.nodes, node)
		m.depths = append(m.depths, v.depth)
		children := m.children[v.id]
		for i := len(children) - 1; i >= 0; i-- {
			stack = append(stack, visit{children[i], v.depth + 1})
		}
	}
	m.tree.Current = nearest[tree.Current]
	m.selected = m.indices[m.tree.Current]
	m.start = max(0, min(m.selected-historyWindowRows/2, len(m.nodes)-historyWindowRows))
	m.Window.Title = "User inputs · " + strings.Join(strings.Fields(render.Clean(tree.Name)), " ")
	m.Window.Hint = "↑↓ ←→ navigate · Enter undo to input · Space inspect · Esc closes"
	m.update()
	return m
}

func (m *historyMenu) update() {
	if len(m.nodes) == 0 {
		m.Window.Text = "No user inputs"
		return
	}
	m.selected = max(0, min(m.selected, len(m.nodes)-1))
	if m.selected < m.start || m.selected >= m.start+historyWindowRows {
		m.start = max(0, min(m.selected-historyWindowRows/2, len(m.nodes)-historyWindowRows))
		m.Window.Scroll = 0
	}
	end := min(len(m.nodes), m.start+historyWindowRows)
	m.rows = m.rows[:0]
	for i := m.start; i < end; i++ {
		node := m.nodes[i]
		indent := strings.Repeat("  ", min(m.depths[i], 8))
		if m.depths[i] > 8 {
			indent += "… "
		}
		state := ""
		if node.ID == m.tree.Current {
			state = " · current"
		} else if !node.Restorable {
			state = " · inspect"
		}
		label := node.Label
		if label == "" {
			label = "(attachment)"
		}
		m.rows = append(m.rows, menuRow(fmt.Sprintf("%s↳ #%d · %s%s", indent, node.ID, label, state), i == m.selected))
	}
	node := m.nodes[m.selected]
	m.Window.Header = fmt.Sprintf("Inputs %d–%d of %d", m.start+1, end, len(m.nodes))
	if node.Reason != "" {
		m.Window.Header += "\nInspect only: " + node.Reason
	}
	m.Window.Text = strings.Join(m.rows, "\n")
}

func (m *historyMenu) key(ev *tcell.EventKey, height int) (action string, id int64) {
	if ev.Key() == tcell.KeyEscape {
		return "close", 0
	}
	if len(m.nodes) == 0 {
		return "", 0
	}
	node := m.nodes[m.selected]
	switch ev.Key() {
	case tcell.KeyUp:
		m.selected--
	case tcell.KeyDown:
		m.selected++
	case tcell.KeyHome:
		m.selected = 0
	case tcell.KeyEnd:
		m.selected = len(m.nodes) - 1
	case tcell.KeyLeft:
		if index, found := m.indices[node.Parent]; found {
			m.selected = index
		}
	case tcell.KeyRight:
		if children := m.children[node.ID]; len(children) > 0 {
			m.selected = m.indices[children[0]]
		}
	case tcell.KeyPgUp, tcell.KeyCtrlU:
		m.selected -= max(1, height/2)
	case tcell.KeyPgDn, tcell.KeyCtrlD:
		m.selected += max(1, height/2)
	case tcell.KeyEnter:
		if node.Restorable {
			return "restore", m.targets[node.ID]
		}
		return "inspect", node.ID
	case tcell.KeyRune:
		if ev.Rune() == ' ' {
			return "inspect", node.ID
		}
	}
	m.update()
	return "", 0
}

func (m *historyMenu) reveal(width, height int) {
	if height <= 0 || len(m.nodes) == 0 {
		return
	}
	row := 0
	for _, line := range m.rows[:m.selected-m.start] {
		row += len(wrap(line, max(1, width)))
	}
	if row < m.Window.Scroll {
		m.Window.Scroll = row
	} else if row >= m.Window.Scroll+height {
		m.Window.Scroll = row - height + 1
	}
}

func (m *historyMenu) mouse(ev *tcell.EventMouse, w, h int) {
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
	target := m.Window.Scroll + y - top - headers - 1
	row := 0
	for i, line := range m.rows {
		next := row + len(wrap(line, inner))
		if target >= row && target < next {
			m.selected = m.start + i
			m.update()
			return
		}
		row = next
	}
}
