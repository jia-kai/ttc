// Package tui contains a Linux terminal frontend and one reusable inspection window.
package tui

import (
	"fmt"
	"os"
	"strings"
	"ttc/internal/render"

	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v3"
	"github.com/mattn/go-runewidth"
)

// Window displays messages, tool records, system prompts, and command results using the same widget.
type Window struct {
	actor, subagentName   string // Same attribution as the conversation card.
	Title, Text           string
	SourceAgent           string // Display name in a fixed provenance row, separate from view-specific headers.
	Header                string // Optional fixed view metadata or question tabs above scrolling content.
	HeaderFocus           string // Compact view summary shown when the whole header cannot fit.
	Hint                  string // Optional title-bar key hint; empty uses "Esc closes".
	HideHint              bool   // Suppress title-bar hints when controls are described in the body.
	Scroll                int
	System                bool                   // Distinct color for inspected system prompts/runtime messages.
	Markdown              bool                   // Render portable Markdown for messages, tool details and command results.
	Styled                bool                   // Trusted frontend SGR; source text must be sanitized before styling.
	Structured            bool                   // Plain inspection source; highlighted transiently, never Markdown-interpreted.
	structuredState       render.StructuredState // Source syntax hints at this page's start.
	CallID                string                 // Transient tool-card identity; cleared when its final record arrives.
	completionEntry       int64                  // Original final-result envelope; D opens its status/warning/tool metadata.
	boundaryRaw           int64                  // Explicit alternate raw-wire action for a readable boundary inspector.
	JobID                 string                 // Live inspector capture; never set when replaying stored history.
	TimerID               string                 // Live timer inspector; never recreated from stored history.
	Detail                string                 // Base transient detail, without the expanded capture tail.
	cachedText            string
	cachedWidth           int
	cachedLines           []string
	cachedMode            uint8
	cachedColors          bool
	cachedStructuredState render.StructuredState
	assets                []displayRow // Parallel to cachedLines; only viewport assets are requested.
	renderer              *imageRenderer
	cachedRevision        uint64
	pages                 *inspectionPages           // Optional retained history pager; only the current page is rendered.
	layoutBody            func(width int) windowBody // Optional prewrapped interactive body; invalidated by its owner.
	body                  windowBody
}

type cellPosition struct{ x, y int }

// windowBody uses body-relative display cells, before viewport scrolling.
type windowBody struct {
	lines    []string
	cursor   *cellPosition // Nil when the body is not accepting text.
	focusRow int
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

// Lines returns the scrolled body viewport, wrapping text or using layoutBody.
// Width is in terminal cells and height is in display rows; Scroll is clamped.
func (w *Window) Lines(width, height int) []string {
	height = max(0, height)
	lines := w.currentLines(width)
	w.Scroll = min(max(0, w.Scroll), max(0, len(lines)-height))
	return lines[w.Scroll:min(len(lines), w.Scroll+height)]
}

// currentLines refreshes the current page's render cache without loading pages.
func (w *Window) currentLines(width int) []string {
	mode := uint8(0)
	if w.Markdown {
		mode |= 1
	}
	if w.Styled {
		mode |= 2
	}
	if w.Structured {
		mode |= 4
	}
	colors := os.Getenv("NO_COLOR") == ""
	revision := uint64(0)
	if w.renderer != nil {
		revision = w.renderer.revision
	}
	if w.cachedLines == nil || w.cachedText != w.Text || w.cachedWidth != width || w.cachedRevision != revision || w.cachedMode != mode || w.cachedColors != colors || w.cachedStructuredState != w.structuredState {
		w.cachedMode, w.cachedColors = mode, colors
		w.cachedStructuredState = w.structuredState
		w.cachedText = w.Text
		w.cachedWidth = width
		w.cachedRevision = revision
		w.assets = nil
		if w.layoutBody != nil {
			w.body = w.layoutBody(max(1, width))
			w.cachedLines = w.body.lines
		} else if w.Structured {
			if colors {
				text, _ := render.StructuredPage(w.Text, w.structuredState, true)
				w.cachedLines = wrapStyled(text, width)
			} else {
				// styledRows restores its pen after each newline, including an
				// empty pen's reset. Keep the no-color source fully control-free.
				w.cachedLines = wrap(render.Clean(w.Text), width)
			}
		} else if w.Markdown && w.renderer != nil {
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
	return w.cachedLines
}

// ScrollBy scrolls by display rows using the most recently rendered width.
// For paged windows, continuing at a boundary loads at most one adjacent page:
// forwards starts at its top; backwards ends at its bottom. It returns true only
// when a page was loaded. Failures retain the old page and appear in Header.
// A zero delta or nonpositive viewport height never loads a page.
func (w *Window) ScrollBy(delta, height int) bool {
	if w.pages == nil {
		w.Scroll += delta
		return false
	}
	if delta == 0 || height <= 0 {
		return false
	}
	width := max(1, w.cachedWidth)
	bottom := max(0, len(w.currentLines(width))-height)
	w.Scroll = min(max(0, w.Scroll), bottom)
	if delta > 0 && w.Scroll == bottom || delta < 0 && w.Scroll == 0 {
		if !w.movePage(delta > 0) {
			return false
		}
		if delta < 0 {
			w.Scroll = max(0, len(w.currentLines(width))-height)
		}
		return true
	}
	// Bound the delta before adding so even very large input cannot overflow
	// or skip a page. Reaching the boundary is not itself a page transition.
	if delta > 0 {
		w.Scroll += min(delta, bottom-w.Scroll)
	} else {
		w.Scroll += max(delta, -w.Scroll)
	}
	return false
}

// Key handles scrolling; false means the caller should handle the key.
func (w *Window) Key(ev *tcell.EventKey, height int) bool {
	if ev.Key() == tcell.KeyRune && w.pageKey(ev.Str()) {
		return true
	}
	switch {
	case ev.Key() == tcell.KeyUp:
		w.ScrollBy(-1, height)
	case ev.Key() == tcell.KeyDown:
		w.ScrollBy(1, height)
	case ev.Key() == tcell.KeyPgUp:
		w.ScrollBy(-max(1, height), height)
	case ev.Key() == tcell.KeyPgDn:
		w.ScrollBy(max(1, height), height)
	case isCtrl(ev, "u"):
		w.ScrollBy(-max(1, height/2), height)
	case isCtrl(ev, "d"):
		w.ScrollBy(max(1, height/2), height)
	case ev.Key() == tcell.KeyHome:
		w.Scroll = 0
	case ev.Key() == tcell.KeyEnd:
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
