// Package tui contains a Linux terminal frontend and one reusable inspection window.
package tui

import (
	"fmt"
	"scicode/internal/render"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
)

// Window displays messages, tool records, system prompts, and command results using the same widget.
type Window struct {
	Title, Text string
	Header      string // Optional fixed header, used for question tabs above scrolling content.
	HeaderFocus string // Active header label shown alone when the whole header cannot fit.
	Hint        string // Optional title-bar key hint; empty uses "Esc closes".
	Scroll      int
	System      bool   // Distinct color for inspected system prompts/runtime messages.
	Markdown    bool   // Render portable Markdown for messages, tool details and command results.
	CallID      string // Transient tool-card identity; cleared when its final record arrives.
	JobID       string // Live inspector capture; never set when replaying stored history.
	Detail      string // Base transient detail, without the expanded capture tail.
	cachedText  string
	cachedWidth int
	cachedLines []string
}

// HeaderLines wraps the fixed header, leaving layout ownership with the frontend.
func (w *Window) HeaderLines(width, limit int) []string {
	if w.Header == "" || limit <= 0 {
		return nil
	}
	lines := wrap(render.Clean(w.Header), width)
	if len(lines) > limit && w.HeaderFocus != "" {
		lines = wrap(render.Clean(w.HeaderFocus), width)
	}
	return lines[:min(len(lines), limit)]
}

// Lines wraps portable Markdown as readable text within a terminal viewport.
func (w *Window) Lines(width, height int) []string {
	height = max(0, height)
	if w.cachedLines == nil || w.cachedText != w.Text || w.cachedWidth != width {
		w.cachedText = w.Text
		w.cachedWidth = width
		if w.Markdown {
			text, err := render.Terminal(w.Text, max(1, width))
			if err != nil {
				w.cachedLines = wrap(render.Clean(w.Text), width)
			} else {
				w.cachedLines = styledRows(strings.Trim(text, "\n"))
			}
		} else {
			w.cachedLines = wrap(render.Clean(w.Text), width)
		}
	}
	lines := w.cachedLines
	if w.Scroll < 0 {
		w.Scroll = 0
	}
	max := len(lines) - height
	if max < 0 {
		max = 0
	}
	if w.Scroll > max {
		w.Scroll = max
	}
	end := w.Scroll + height
	if end > len(lines) {
		end = len(lines)
	}
	return lines[w.Scroll:end]
}

// Key handles scrolling; false means the caller should handle the key.
func (w *Window) Key(ev *tcell.EventKey, height int) bool {
	height = max(1, height)
	switch ev.Key() {
	case tcell.KeyUp:
		w.Scroll--
	case tcell.KeyDown:
		w.Scroll++
	case tcell.KeyPgUp:
		w.Scroll -= height
	case tcell.KeyPgDn:
		w.Scroll += height
	case tcell.KeyCtrlU:
		w.Scroll -= max(1, height/2)
	case tcell.KeyCtrlD:
		w.Scroll += max(1, height/2)
	case tcell.KeyHome:
		w.Scroll = 0
	case tcell.KeyEnd:
		w.Scroll = 1 << 30
	default:
		return false
	}
	return true
}

// scrollIndicator describes wrapped display rows and viewport progress. Narrow
// panes show only the percentage; a fully visible document is at 100 percent.
func scrollIndicator(total, height, offset, width int) string {
	span := max(0, total-max(0, height))
	offset = min(max(0, offset), span)
	percent := 100
	if span > 0 {
		percent = offset * 100 / span
	}
	first := 0
	if total > 0 && height > 0 {
		first = offset + 1
	}
	text := fmt.Sprintf("lines %d–%d/%d · %d%%", first, min(total, offset+max(0, height)), total, percent)
	if runewidth.StringWidth(text) > width {
		text = fmt.Sprintf("%d%%", percent)
	}
	if runewidth.StringWidth(text) > width {
		return ""
	}
	return text
}
func wrap(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	out := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\t", "    "), "\n") {
		var b strings.Builder
		cells := 0
		for _, r := range line {
			n := runewidth.RuneWidth(r)
			if cells+n > width && cells > 0 {
				out = append(out, b.String())
				b.Reset()
				cells = 0
			}
			b.WriteRune(r)
			cells += n
		}
		out = append(out, b.String())
	}
	return out
}
