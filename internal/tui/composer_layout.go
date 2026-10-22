package tui

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

const maxComposerRows = 6

// composerRow maps one visual row to its original byte/rune offsets. Wrapped
// rows never insert newlines into the draft. Display text contains no controls.
type composerRow struct {
	text                 string
	byteStart, runeStart int
}

type composerLayout struct {
	text  string
	width int
	rows  []composerRow
}

type composerViewport struct {
	rows             []composerRow
	top, total       int
	cursorX, cursorY int // Cells relative to the visible input; Y is -1 when hidden.
}

func visibleInput(text string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return '⇥'
		}
		if unicode.IsControl(r) {
			return '�'
		}
		return r
	}, text)
}

func (c *composer) wrapped(width int) *composerLayout {
	width = max(1, width)
	if c.layout != nil && c.layout.width == width && c.layout.text == c.text {
		return c.layout
	}
	layout := &composerLayout{text: c.text, width: width}
	var row strings.Builder
	byteAt, runeAt, cells := 0, 0, 0
	startByte, startRune := 0, 0
	finish := func() {
		layout.rows = append(layout.rows, composerRow{row.String(), startByte, startRune})
		row.Reset()
		cells = 0
		startByte, startRune = byteAt, runeAt
	}
	for byteAt < len(c.text) {
		if c.text[byteAt] == '\n' {
			finish()
			byteAt++
			runeAt++
			startByte, startRune = byteAt, runeAt
			continue
		}
		r, size := utf8.DecodeRuneInString(c.text[byteAt:])
		cluster := c.text[byteAt : byteAt+size]
		display, widthCells := "", 1
		if unicode.IsControl(r) {
			display = visibleInput(cluster)
		} else {
			cluster, widthCells = ansi.FirstGraphemeCluster(c.text[byteAt:], ansi.GraphemeWidth)
			display = cluster
		}
		if widthCells > width {
			// A wide grapheme cannot fit a one-cell pane. Keep the source intact.
			display, widthCells = "�", 1
		}
		if cells+widthCells > width {
			finish()
		}
		row.WriteString(display)
		cells += widthCells
		byteAt += len(cluster)
		runeAt += utf8.RuneCountInString(cluster)
	}
	finishFull := cells == width
	finish()
	if finishFull {
		// The insertion cursor needs a cell after an exactly full final row.
		finish()
	}
	c.layout = layout
	return layout
}

// cursorPosition maps a source rune offset to display cells, independently of
// scrolling. Both the main input and modal editors use this layout.
func (layout *composerLayout) cursorPosition(cursor int) (x, y int) {
	rows, width := layout.rows, layout.width
	cursorRow := max(0, sort.Search(len(rows), func(i int) bool { return rows[i].runeStart > cursor })-1)
	row := rows[cursorRow]
	prefix := layout.text[row.byteStart:]
	remaining := cursor - row.runeStart
	byteEnd := 0
	for remaining > 0 && byteEnd < len(prefix) {
		_, size := utf8.DecodeRuneInString(prefix[byteEnd:])
		byteEnd += size
		remaining--
	}
	x = ansi.StringWidth(visibleInput(prefix[:byteEnd]))
	if width > 0 && x >= width && cursorRow+1 < len(rows) {
		// A hard newline after an exactly full row has no cell on that row.
		// Its insertion point is the following row's start, not the last glyph.
		cursorRow++
		x = 0
	}
	x = min(x, max(0, width-1))
	return x, cursorRow
}

func (c *composer) viewport(width, height int) composerViewport {
	layout := c.wrapped(width)
	rows := layout.rows
	x, cursorRow := layout.cursorPosition(c.cursor)
	height = min(max(0, height), len(rows))
	top := max(0, cursorRow-height+1)
	if c.manualScroll {
		top = c.scrollTop
	}
	top = min(max(0, top), max(0, len(rows)-height))
	c.scrollTop = top
	y := cursorRow - top
	if y < 0 || y >= height || width <= 0 {
		y = -1
	}
	return composerViewport{rows: rows[top : top+height], top: top, total: len(rows), cursorX: x, cursorY: y}
}

// inputGeometry is shared by drawing, steering-preview limits and input scrolling.
// Its rows are disjoint even in tiny panes; normal mode reserves a transcript row
// whenever there is room after the status, input and footer.
type inputGeometry struct {
	input                                composerViewport
	inputY, transcriptHeight, queuedRows int
}

func layoutInput(c *composer, width, height int, fullscreen bool, pending int) inputGeometry {
	statusRows := 0
	if !fullscreen && height >= 3 {
		statusRows = 1
	}
	transcriptRows := 0
	if height >= 3+statusRows {
		transcriptRows = 1
	}
	limit := min(maxComposerRows, max(0, height-1-statusRows-transcriptRows))
	if fullscreen && c.text == "" {
		limit = 0
	}
	input := c.viewport(max(0, width-2), limit)
	y := max(0, height-1-len(input.rows))
	queued := 0
	if !fullscreen {
		queued = min(pending, max(0, y-statusRows-transcriptRows))
	}
	return inputGeometry{input: input, inputY: y, transcriptHeight: max(0, y-statusRows-queued), queuedRows: queued}
}

func (c *frontendController) scrollInput(delta int) {
	w, h := c.screen.Size()
	w = c.sidebar.bounds(w, h, c.fullscreen)
	geometry := layoutInput(&c.draft, w, h, c.fullscreen, 0)
	c.draft.scrollTop = min(max(0, geometry.input.top+delta), max(0, geometry.input.total-len(geometry.input.rows)))
	c.draft.manualScroll = true
}
