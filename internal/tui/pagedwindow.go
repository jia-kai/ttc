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
	fences map[int]string // Small language hints only, never cached body text.
}

// NewPagedWindow displays retained history without loading or wrapping the whole
// record. Load returns one bounded Unicode-character page at the given offset.
func NewPagedWindow(page history.InspectionPage, load func(int) (history.InspectionPage, error)) *Window {
	w := &Window{Title: page.Title, Markdown: page.Markdown, System: page.System, Hint: "[ ] pages · arrows scroll · Esc closes", pages: &inspectionPages{page: page, load: load, fences: map[int]string{0: ""}}}
	w.setPage(page)
	return w
}

func (w *Window) setPage(page history.InspectionPage) {
	p := w.pages
	p.page = page
	w.Scroll = 0
	w.Text = page.Text
	if page.JSON {
		w.Text = render.Fence(page.Text, "json")
		w.Markdown = true
	} else if page.Markdown {
		fence := p.fences[page.Offset]
		if fence != "" {
			w.Text = "```" + strings.TrimPrefix(fence, "`") + "\n" + w.Text
		}
		for _, line := range strings.Split(page.Text, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				if fence != "" {
					fence = ""
				} else {
					language := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "```"))
					if len(language) > 64 {
						language = ""
					}
					fence = "`" + language
				}
			}
		}
		p.fences[page.Offset+page.Limit] = fence
	}
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
