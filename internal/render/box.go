package render

import (
	"iter"
	"strings"
	"unicode"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// FixedBox is an atomic terminal rectangle. Width is in terminal cells; Rows
// contain ANSI text no wider than Width. Baseline is a zero-based row index.
// Callers own Rows and must not mutate them during layout. Empty boxes are ignored.
// A box wider than the available width overflows intact, never being clipped.
type FixedBox struct {
	Rows     []string
	Width    int
	Baseline int
}

// flowItem contains either styled text or a box factory. The factory receives
// the full line/cell width (at least one), not the current line's remaining room.
// block boxes occupy their own lines. Text may contain explicit newlines.
type flowItem struct {
	text  string
	box   func(width int) FixedBox
	block bool
}

type flowToken struct {
	text  string
	width int
	space bool
	line  bool
}

// flowTokens splits only at grapheme boundaries and retains complete ANSI
// sequences. SGR state is replayed for each token, isolating styles from boxes,
// padding, and other table cells after tokens are moved onto different rows.
func flowTokens(input string) iter.Seq[flowToken] {
	return func(yield func(flowToken) bool) {
		text := input
		var buf, controls strings.Builder
		// Keep the current attributes, not their history: replaying every previous
		// SGR sequence at every word makes repeated partial resets quadratic.
		var style uv.Style
		stylePrefix := ""
		var parser ansi.Parser
		parser.SetParamsSize(32)
		width, kind := 0, -1
		flush := func() bool {
			if kind < 0 {
				return true
			}
			s := buf.String()
			if strings.Contains(s, "\x1b[") {
				s += ansi.ResetStyle
			}
			token := flowToken{text: s, width: width, space: kind == 1}
			buf.Reset()
			width, kind = 0, -1
			return yield(token)
		}
		var state byte
		for len(text) > 0 {
			seq, w, n, next := ansi.DecodeSequence(text, state, &parser)
			text, state = text[n:], next
			if w == 0 && seq != "\n" && seq != "\t" && seq != "\r" && (seq[0] == '\x1b' || seq[0] < 0x20 || seq[0] == 0x7f) {
				sgr := ansi.HasCsiPrefix(seq) && parser.Command() == 'm'
				if sgr {
					uv.ReadStyle(parser.Params(), &style)
					stylePrefix = ""
					if !style.IsZero() {
						stylePrefix = style.String()
					}
				}
				if kind >= 0 {
					buf.WriteString(seq)
				} else if !sgr {
					controls.WriteString(seq)
				}
				continue
			}
			if seq == "\n" {
				if !flush() || !yield(flowToken{line: true}) {
					return
				}
				continue
			}
			if seq == "\r" {
				continue
			}
			k := 0
			if unicode.IsSpace([]rune(seq)[0]) {
				k = 1
				if seq == "\t" {
					seq, w = "    ", 4
				}
			}
			if kind != k {
				if !flush() {
					return
				}
				kind = k
				buf.WriteString(stylePrefix)
				buf.WriteString(controls.String())
				controls.Reset()
			}
			buf.WriteString(seq)
			width += w
		}
		flush()
	}
}

// layoutFlow greedily packs words and atomic boxes, aligning inline baselines.
// Whitespace separating wrapped words is discarded; explicit newlines are kept.
// Long words wrap by grapheme. A single grapheme or box may exceed width when it
// cannot fit even on an empty line. Nonpositive widths are treated as one cell.
func layoutFlow(items []flowItem, width int) []string {
	return layoutFlowBox(items, width).Rows
}

// layoutFlowBox retains the first line's baseline for container decorations.
func layoutFlowBox(items []flowItem, width int) FixedBox {
	width = max(1, width)
	var rows []string
	baseline := 0
	var line []FixedBox
	used, above, below := 0, 0, 0
	pending := ""
	pendingWidth := 0
	flush := func(force bool) {
		if len(line) == 0 {
			if force {
				rows = append(rows, "")
			}
			return
		}
		if len(rows) == 0 {
			baseline = above
		}
		for y := 0; y <= above+below; y++ {
			var row strings.Builder
			for _, b := range line {
				i := y - above + b.Baseline
				s := ""
				if i >= 0 && i < len(b.Rows) {
					s = b.Rows[i]
				}
				row.WriteString(s)
				row.WriteString(strings.Repeat(" ", max(0, b.Width-ansi.StringWidth(s))))
			}
			rows = append(rows, row.String())
		}
		line, used, above, below = nil, 0, 0, 0
	}
	add := func(b FixedBox) {
		if len(b.Rows) == 0 {
			return
		}
		b.Baseline = min(max(0, b.Baseline), len(b.Rows)-1)
		b.Width = max(0, b.Width)
		for _, row := range b.Rows {
			b.Width = max(b.Width, ansi.StringWidth(row))
		}
		if used > 0 && used+pendingWidth+b.Width > width {
			flush(false)
			pending, pendingWidth = "", 0
		}
		if pendingWidth > 0 && used > 0 {
			line = append(line, FixedBox{Rows: []string{pending}, Width: pendingWidth})
			used += pendingWidth
		}
		pending, pendingWidth = "", 0
		line = append(line, b)
		used += b.Width
		above = max(above, b.Baseline)
		below = max(below, len(b.Rows)-b.Baseline-1)
	}
	for i := 0; i < len(items); i++ {
		item := items[i]
		if item.box != nil {
			b := item.box(width)
			if item.block {
				flush(false)
				pending, pendingWidth = "", 0
				add(b)
				flush(false)
			} else {
				add(b)
			}
			continue
		}
		// Adjacent styled text must wrap as one stream, not as separate words.
		var text strings.Builder
		text.WriteString(item.text)
		for i+1 < len(items) && items[i+1].box == nil {
			i++
			text.WriteString(items[i].text)
		}
		for t := range flowTokens(text.String()) {
			if t.line {
				flush(true)
				pending, pendingWidth = "", 0
			} else if t.space {
				pending += t.text
				pendingWidth += t.width
			} else if t.width <= width {
				add(FixedBox{Rows: []string{t.text}, Width: t.width})
			} else {
				// Decode rather than cutting at cell offsets: a two-cell
				// grapheme still has to survive a one-cell viewport intact.
				var wrapped strings.Builder
				x := 0
				text := t.text
				var state byte
				for len(text) > 0 {
					seq, w, n, next := ansi.DecodeSequence(text, state, nil)
					text, state = text[n:], next
					if w > 0 && x > 0 && x+w > width {
						wrapped.WriteByte('\n')
						x = 0
					}
					wrapped.WriteString(seq)
					x += w
				}
				for part := range flowTokens(wrapped.String()) {
					if part.line {
						flush(false)
					} else {
						add(FixedBox{Rows: []string{part.text}, Width: part.width})
					}
				}
			}
		}
	}
	flush(false)
	return FixedBox{Rows: rows, Width: width, Baseline: baseline}
}

// layoutTable lays out [row][column][flow item] cells; the first row is the
// header. Alignments are -1/0/1 for left/center/right (missing entries are left).
// Columns shrink to their widest indivisible grapheme/box. If those minima and
// separators cannot fit, cells are stacked in row-major order without clipping.
func layoutTable(cells [][][]flowItem, alignments []int, width int) []string {
	width = max(1, width)
	n := 0
	for _, row := range cells {
		n = max(n, len(row))
	}
	if n == 0 {
		return nil
	}
	minimum, natural := make([]int, n), make([]int, n)
	for _, row := range cells {
		for c, cell := range row {
			minimum[c] = max(minimum[c], 1)
			lineWidth, widest := 0, 0
			for _, item := range cell {
				if item.box != nil {
					b := item.box(1)
					minimum[c] = max(minimum[c], b.Width)
					lineWidth += item.box(width).Width
					continue
				}
				var state byte
				text := item.text
				for len(text) > 0 {
					seq, w, count, next := ansi.DecodeSequence(text, state, nil)
					text, state = text[count:], next
					if seq == "\n" {
						widest, lineWidth = max(widest, lineWidth), 0
					} else {
						minimum[c] = max(minimum[c], w)
						lineWidth += w
					}
				}
			}
			natural[c] = max(natural[c], minimum[c], widest, lineWidth)
		}
	}
	total := 3 * (n - 1)
	for _, m := range minimum {
		total += max(1, m)
	}
	if total > width {
		var rows []string
		for r, row := range cells {
			if r > 0 {
				rows = append(rows, "")
			}
			for _, cell := range row {
				rows = append(rows, layoutFlow(cell, width)...)
			}
		}
		return rows
	}
	widths := append([]int(nil), minimum...)
	// Water-fill columns using start/stop events, rather than searching every
	// column for every extra cell. Work and storage are bounded by the actual
	// table output width, even when the viewport is much wider than its content.
	naturalTotal := 3 * (n - 1)
	for _, w := range natural {
		naturalTotal += w
	}
	target := min(width, naturalTotal)
	events := make([]int, target+1)
	for c, m := range minimum {
		if m < natural[c] {
			events[m]++
			events[min(target, natural[c])]--
		}
	}
	active, level := 0, 0
	for ; level < target; level++ {
		active += events[level]
		if active > target-total {
			break
		}
		total += active
	}
	for c := range widths {
		widths[c] = max(minimum[c], min(natural[c], level))
	}
	for c := 0; total < target && c < n; c++ {
		if widths[c] == level && widths[c] < natural[c] {
			widths[c]++
			total++
		}
	}
	var rows []string
	for r, row := range cells {
		rendered := make([][]string, n)
		height := 1
		for c, cell := range row {
			rendered[c] = layoutFlow(cell, widths[c])
			height = max(height, len(rendered[c]))
		}
		for y := 0; y < height; y++ {
			parts := make([]string, n)
			for c := range parts {
				s := ""
				if y < len(rendered[c]) {
					s = rendered[c][y]
				}
				padding := max(0, widths[c]-ansi.StringWidth(s))
				left := 0
				if c < len(alignments) {
					if alignments[c] == 0 {
						left = padding / 2
					} else if alignments[c] == 1 {
						left = padding
					}
				}
				parts[c] = strings.Repeat(" ", left) + s + strings.Repeat(" ", padding-left)
			}
			rows = append(rows, strings.Join(parts, " │ "))
		}
		if r == 0 && len(cells) > 1 {
			parts := make([]string, n)
			for c, w := range widths {
				parts[c] = strings.Repeat("─", w)
			}
			rows = append(rows, strings.Join(parts, "─┼─"))
		}
	}
	return rows
}
