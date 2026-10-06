// Package tui contains a Linux terminal frontend and one reusable inspection window.
package tui

import (
	"fmt"
	"strings"
	"ttc/internal/render"

	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
)

// Window displays messages, tool records, system prompts, and command results using the same widget.
type Window struct {
	actor, subagentName string // Same attribution as the conversation card.
	Title, Text         string
	SourceAgent         string // Display name in a fixed provenance row, separate from view-specific headers.
	Header              string // Optional fixed view metadata or question tabs above scrolling content.
	HeaderFocus         string // Compact view summary shown when the whole header cannot fit.
	Hint                string // Optional title-bar key hint; empty uses "Esc closes".
	HideHint            bool   // Suppress title-bar hints when controls are described in the body.
	Scroll              int
	System              bool   // Distinct color for inspected system prompts/runtime messages.
	Markdown            bool   // Render portable Markdown for messages, tool details and command results.
	Styled              bool   // Trusted frontend SGR; source text must be sanitized before styling.
	CallID              string // Transient tool-card identity; cleared when its final record arrives.
	JobID               string // Live inspector capture; never set when replaying stored history.
	TimerID             string // Live timer inspector; never recreated from stored history.
	Detail              string // Base transient detail, without the expanded capture tail.
	cachedText          string
	cachedWidth         int
	cachedLines         []string
	assets              []displayRow // Parallel to cachedLines; only viewport assets are requested.
	renderer            *imageRenderer
	cachedRevision      uint64
	pages               *inspectionPages // Optional retained history pager; only the current page is rendered.
}

// HeaderLines wraps the fixed header, leaving layout ownership with the frontend.
func (w *Window) HeaderLines(width, limit int) []string {
	if limit <= 0 || width <= 0 {
		return nil
	}
	var lines []string
	if w.SourceAgent != "" {
		// Provenance must not consume the body viewport in a narrow pane.
		name := strings.Join(strings.Fields(render.Clean(w.SourceAgent)), " ")
		lines = append(lines, ansi.Truncate("Source agent: "+name, width, "…"))
	}
	if w.Header != "" && len(lines) < limit {
		available := limit - len(lines)
		if w.TimerID != "" {
			// Reserve useful scrolling space for startup parameters in short panes.
			available = min(available, max(2, limit/2))
		}
		header := wrap(render.Clean(w.Header), width)
		if len(header) > available && w.HeaderFocus != "" {
			header = wrap(render.Clean(w.HeaderFocus), width)
		}
		lines = append(lines, header[:min(len(header), available)]...)
	}
	return lines
}

// Lines wraps portable Markdown as readable text within a terminal viewport.
func (w *Window) Lines(width, height int) []string {
	height = max(0, height)
	revision := uint64(0)
	if w.renderer != nil {
		revision = w.renderer.revision
	}
	if w.cachedLines == nil || w.cachedText != w.Text || w.cachedWidth != width || w.cachedRevision != revision {
		w.cachedText = w.Text
		w.cachedWidth = width
		w.cachedRevision = revision
		w.assets = nil
		if w.Markdown && w.renderer != nil {
			w.assets = w.renderer.layout(line{text: w.Text, markdown: true}, max(1, width))
			w.cachedLines = make([]string, len(w.assets))
			for i, row := range w.assets {
				w.cachedLines[i] = row.text
			}
		} else if w.Markdown {
			text, err := render.Terminal(w.Text, max(1, width))
			if err != nil {
				w.cachedLines = wrap(render.Clean(w.Text), width)
			} else {
				w.cachedLines = styledRows(strings.Trim(text, "\n"))
			}
		} else if w.Styled {
			w.cachedLines = wrapStyled(w.Text, width)
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
	if ev.Key() == tcell.KeyRune && w.pageKey(ev.Rune()) {
		return true
	}
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
