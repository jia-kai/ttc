package tui

import (
	"fmt"
	"strings"

	"ttc/internal/llm"

	"github.com/gdamore/tcell/v3"
)

// modelMenu chooses an exact catalog model followed by one of its reasoning variants.
// Its content uses the same Window as message inspectors; it owns only selection state.
type modelMenu struct {
	Window       Window
	models       []llm.ModelSpec
	current      llm.Selection
	modelIndex   int
	variantIndex int
	variants     bool
	rows         []string
	manualScroll bool
	easy         *llm.Selection
}

func newModelMenu(models []llm.ModelSpec, current llm.Selection) *modelMenu {
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
	if isCtrl(ev, "d") || isCtrl(ev, "u") || ev.Key() == tcell.KeyPgUp || ev.Key() == tcell.KeyPgDn {
		m.Window.Key(ev, height)
		m.manualScroll = true
		return "", "", false
	}
	m.manualScroll = false
	// E on a family opens its variants; E on a variant is handled by the
	// controller because saving the preference can fail.
	if !m.variants && ev.Key() == tcell.KeyRune && ev.Modifiers() == 0 && (ev.Str() == "e" || ev.Str() == "E") {
		ev = tcell.NewEventKey(tcell.KeyEnter, "", 0)
	}
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
		m.Window.Title = "Reasoning variant"
		m.Window.Hint = "E toggles easy · Enter main · Esc back · " + model.Name
		for i, v := range model.Variants {
			label := v
			if description := model.VariantDescriptions[v]; description != "" {
				label += " · " + description
			}
			if v == model.DefaultVariant {
				label += " (default)"
			}
			easy := m.easy != nil && model.ID == m.easy.Model.ID && v == m.easy.Variant
			selected := model.ID == m.current.Model.ID && v == m.current.Variant
			m.rows = append(m.rows, modelMenuRow(label, i == m.variantIndex, easy, selected))
		}
	} else {
		m.Window.Title = "Model family"
		m.Window.Hint = "E/Enter variants · E toggles easy there"
		for i, model := range m.models {
			label := fmt.Sprintf("%s · %s", model.Name, model.ID)
			if model.Description != "" {
				label += " · " + model.Description
			}
			easy := m.easy != nil && model.ID == m.easy.Model.ID
			selected := model.ID == m.current.Model.ID
			m.rows = append(m.rows, modelMenuRow(label, i == m.modelIndex, easy, selected))
		}
	}
	m.Window.Text = strings.Join(m.rows, "\n")
}

func modelMenuRow(label string, focused, easy, selected bool) string {
	marker := "  "
	if easy && selected {
		marker = "E*"
	} else if easy {
		marker = "E "
	} else if selected {
		marker = "* "
	}
	cursor := " "
	if focused {
		cursor = ">"
	}
	return cursor + marker + " " + label
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
