package tui

import (
	"fmt"
	"strings"

	"scicode/internal/provider"

	"github.com/gdamore/tcell/v2"
)

// modelMenu chooses an exact catalog model followed by one of its reasoning variants.
// Its content uses the same Window as message inspectors; it owns only selection state.
type modelMenu struct {
	Window       Window
	models       []provider.ModelSpec
	current      provider.Selection
	modelIndex   int
	variantIndex int
	variants     bool
	rows         []string
	manualScroll bool
}

func newModelMenu(models []provider.ModelSpec, current provider.Selection) *modelMenu {
	m := &modelMenu{models: models, current: current}
	for i, model := range models {
		if model.ID == current.Model.ID {
			m.modelIndex = i
			break
		}
	}
	m.update()
	return m
}

// key returns a completed selection or cancellation. Escape backs out of variants first.
func (m *modelMenu) key(ev *tcell.EventKey, height int) (id, variant string, closed bool) {
	if ev.Key() == tcell.KeyCtrlD || ev.Key() == tcell.KeyCtrlU || ev.Key() == tcell.KeyPgUp || ev.Key() == tcell.KeyPgDn {
		m.Window.Key(ev, height)
		m.manualScroll = true
		return "", "", false
	}
	m.manualScroll = false
	index, count := &m.modelIndex, len(m.models)
	model := m.models[m.modelIndex]
	if m.variants {
		index, count = &m.variantIndex, len(model.Variants)
	}
	switch ev.Key() {
	case tcell.KeyEscape, tcell.KeyLeft:
		if m.variants {
			m.variants = false
		} else {
			return "", "", true
		}
	case tcell.KeyUp:
		if *index > 0 {
			*index--
		}
	case tcell.KeyDown:
		if *index+1 < count {
			*index++
		}
	case tcell.KeyHome:
		*index = 0
	case tcell.KeyEnd:
		*index = count - 1
	case tcell.KeyEnter, tcell.KeyRight:
		if m.variants {
			return model.ID, model.Variants[m.variantIndex], true
		}
		m.variants = true
		m.variantIndex = 0
		selected := model.DefaultVariant
		if model.ID == m.current.Model.ID {
			selected = m.current.Variant
		}
		for i, v := range model.Variants {
			if v == selected {
				m.variantIndex = i
				break
			}
		}
	}
	m.update()
	return "", "", false
}

func (m *modelMenu) update() {
	m.rows = nil
	if m.variants {
		model := m.models[m.modelIndex]
		m.Window.Title = "Reasoning variant · " + model.Name
		m.Window.Hint = "Enter selects · Esc back"
		for i, v := range model.Variants {
			label := v
			if description := model.VariantDescriptions[v]; description != "" {
				label += " · " + description
			}
			if v == model.DefaultVariant {
				label += " (default)"
			}
			if model.ID == m.current.Model.ID && v == m.current.Variant {
				label += " (selected)"
			}
			m.rows = append(m.rows, menuRow(label, i == m.variantIndex))
		}
	} else {
		m.Window.Title = "Model family"
		m.Window.Hint = "Enter chooses variants · Esc closes"
		for i, model := range m.models {
			label := fmt.Sprintf("%s · %s", model.Name, model.ID)
			if model.Description != "" {
				label += " · " + model.Description
			}
			if model.ID == m.current.Model.ID {
				label += " (selected)"
			}
			m.rows = append(m.rows, menuRow(label, i == m.modelIndex))
		}
	}
	m.Window.Text = strings.Join(m.rows, "\n")
}

func menuRow(label string, selected bool) string {
	if selected {
		return "> " + label
	}
	return "  " + label
}

// reveal keeps the selected row visible when names or descriptions wrap in narrow panes.
func (m *modelMenu) reveal(width, height int) {
	if height <= 0 || m.manualScroll {
		return
	}
	index := m.modelIndex
	if m.variants {
		index = m.variantIndex
	}
	row := 0
	for _, text := range m.rows[:index] {
		row += len(wrap(text, width))
	}
	if row < m.Window.Scroll {
		m.Window.Scroll = row
	} else if row >= m.Window.Scroll+height {
		m.Window.Scroll = row - height + 1
	}
}
