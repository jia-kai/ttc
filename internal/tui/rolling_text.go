package tui

import (
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v2"
	"scicode/internal/render"
)

// rollingText is a single-line, cell-clipped text widget. Overflow bounces at
// four cells per second with one-second pauses at either end. Its animation
// epoch belongs to the text identity, not refresh count or output activity.
// Callers clear its rectangle before drawing and retain it only while visible.
type rollingText struct {
	source, text string
	glyphs       []textGlyph
	cells        int
	started      time.Time
}

type textGlyph struct {
	runes        []rune
	start, cells int
}

func newRollingText(source string) *rollingText {
	text := strings.ReplaceAll(strings.ReplaceAll(render.Clean(source), "\n", " "), "\t", "    ")
	return &rollingText{source: source, text: text}
}

// draw lazily indexes graphemes, then visits only the visible window. Partial
// wide glyphs are blank rather than split across the widget's cell boundaries.
func (t *rollingText) draw(s tcell.Screen, x, y, width int, now time.Time, style tcell.Style) {
	if width <= 0 {
		return
	}
	if t.glyphs == nil {
		t.glyphs = make([]textGlyph, 0)
		for text := t.text; text != ""; {
			cluster, cells := ansi.FirstGraphemeCluster(text, ansi.GraphemeWidth)
			text = text[len(cluster):]
			if cells > 0 {
				t.glyphs = append(t.glyphs, textGlyph{runes: []rune(cluster), start: t.cells, cells: cells})
				t.cells += cells
			}
		}
	}
	if t.started.IsZero() {
		t.started = now
	}
	offset := rollingTextOffset(now.Sub(t.started), max(0, t.cells-width))
	first := sort.Search(len(t.glyphs), func(i int) bool { return t.glyphs[i].start+t.glyphs[i].cells > offset })
	for _, glyph := range t.glyphs[first:] {
		at := glyph.start - offset
		if at >= width {
			break
		}
		if at < 0 || at+glyph.cells > width {
			continue
		}
		s.SetContent(x+at, y, glyph.runes[0], glyph.runes[1:], style)
	}
}

func rollingTextOffset(elapsed time.Duration, overflow int) int {
	if overflow <= 0 {
		return 0
	}
	const pause = 4
	tick := int(max(0, elapsed) / (250 * time.Millisecond))
	phase := tick % (2*overflow + 2*pause)
	switch {
	case phase < pause:
		return 0
	case phase < pause+overflow:
		return phase - pause
	case phase < 2*pause+overflow:
		return overflow
	default:
		return 2*pause + 2*overflow - phase
	}
}
