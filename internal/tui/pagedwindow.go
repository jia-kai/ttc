package tui

import (
	"fmt"
	"strings"

	"scicode/internal/history"
	"scicode/internal/render"
)

type inspectionPages struct {
	page   history.InspectionPage
	load   func(int) (history.InspectionPage, error)
	fences map[int]inspectionFence // Bounded fence/partial-row hints, never cached body pages.
}

type inspectionFence struct {
	open, partial string
	midRow, plain bool
}

// NewPagedWindow displays retained history without loading or wrapping the whole
// record. Load returns one bounded Unicode-character page at the given offset.
func NewPagedWindow(page history.InspectionPage, load func(int) (history.InspectionPage, error)) *Window {
	w := &Window{Title: page.Title, Markdown: page.Markdown, System: page.System, Hint: "[ ] pages · arrows scroll · Esc closes", pages: &inspectionPages{page: page, load: load, fences: map[int]inspectionFence{0: {}}}}
	w.setPage(page)
	return w
}

func (w *Window) setPage(page history.InspectionPage) {
	p := w.pages
	p.page = page
	w.Scroll = 0
	w.Text = page.Text
	w.Markdown = page.Markdown
	if page.JSON {
		w.Text = render.Fence(page.Text, "json")
		w.Markdown = true
	} else if page.Markdown {
		fence := p.fences[page.Offset]
		next := advanceInspectionFence(page.Text, fence)
		p.fences[page.Offset+page.Limit] = next
		if fence.plain || next.plain {
			w.Markdown = false
			w.Header = "Oversized fence metadata · displaying plain text"
			return
		}
		if fence.midRow && fence.partial == "" {
			// A body-row fragment cannot open or close a fence. Display that
			// fragment literally before resuming Markdown at the next true row.
			fragment, rest, newline := strings.Cut(page.Text, "\n")
			w.Text = render.Fence(fragment, "text")
			if newline {
				if fence.open != "" {
					rest = fence.open + "\n" + rest
				}
				w.Text += "\n" + rest
			}
		} else {
			w.Text = fence.partial + w.Text
			if fence.open != "" {
				w.Text = fence.open + "\n" + w.Text
			}
		}
	}
}

// advanceInspectionFence scans only complete source rows. Partial delimiter
// rows carry across pages; ordinary body rows need only a continuation flag.
// Pathological fence metadata stays bounded and opts into literal presentation.
func advanceInspectionFence(text string, state inspectionFence) inspectionFence {
	const maxHintBytes = 64 << 10
	if state.plain {
		return state
	}
	previousOpen := state.open
	if state.midRow {
		fragment, rest, newline := strings.Cut(text, "\n")
		if state.partial != "" {
			if len(state.partial)+len(fragment) > maxHintBytes {
				return inspectionFence{plain: true}
			}
			row := state.partial + fragment
			if !newline {
				state.partial = row
				return state
			}
			state.open = continuedFence(row, state.open)
		}
		if !newline {
			return state
		}
		text = rest
	}
	state.midRow, state.partial = false, ""
	last := strings.LastIndexByte(text, '\n')
	if last >= 0 {
		state.open = continuedFence(text[:last+1], state.open)
		text = text[last+1:]
	}
	if len(state.open) > maxHintBytes {
		return inspectionFence{plain: true}
	}
	if state.open != previousOpen {
		state.open = strings.Clone(state.open)
	}
	if text != "" {
		state.midRow = true
		candidate := fenceText(text)
		digits := strings.TrimRight(candidate, ".)")
		container := candidate == "-" || candidate == "+" || candidate == "*" ||
			len(digits) > 0 && len(digits) <= 9 && strings.Trim(digits, "0123456789") == ""
		if container || candidate == "" || candidate[0] == '`' || candidate[0] == '~' {
			if len(text) > maxHintBytes {
				return inspectionFence{plain: true}
			}
			state.partial = strings.Clone(text)
		}
	}
	return state
}

func (w *Window) pageKey(key rune) bool {
	if w.pages == nil || key != '[' && key != ']' {
		return false
	}
	p := w.pages
	offset := p.page.Offset
	if key == '[' {
		offset = max(0, offset-p.page.Limit)
	} else {
		offset += p.page.Limit
	}
	if offset == p.page.Offset || offset >= p.page.Total {
		return true
	}
	page, err := p.load(offset)
	if err != nil {
		w.Header = "Inspection failed: " + render.Clean(err.Error())
		return true
	}
	w.Header = ""
	w.setPage(page)
	return true
}

// Progress reports the current bounded page and its row-scroll position.
func (w *Window) Progress(height, width int) string {
	rows := scrollIndicator(len(w.cachedLines), height, w.Scroll, width)
	if w.pages == nil {
		return rows
	}
	p := w.pages.page
	count := max(1, (p.Total+p.Limit-1)/p.Limit)
	page := p.Offset/p.Limit + 1
	text := fmt.Sprintf("page %d/%d · %s", page, count, rows)
	if len([]rune(text)) > width {
		text = fmt.Sprintf("page %d/%d", page, count)
	}
	if len([]rune(text)) > width {
		return ""
	}
	return text
}
