package capture

import (
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"

	"ttc/internal/prompts"
)

const cleanCheckpointBytes = 4096

// MaxCleanReplayBytes bounds source inspected before a selected range. An
// unterminated escape exceeding this budget requires an explicit raw read.
const MaxCleanReplayBytes = 65536

var errCleanReplay = errors.New(prompts.CaptureCleanReplayLimit)

type cleanIndex struct {
	parser, baseParser *ansi.Parser
	checkpoints        []int64 // Ground-state restart offsets, spaced at least 4096 bytes.
	prefix             []byte  // Source preceding the ring base, since its last ground state.
	prefixLost         bool
	lines              []lineCheckpoint
}

type lineCheckpoint struct{ offset, count int64 }

type cleanCell struct {
	r          rune
	generation uint64
}

func newCleanParser() *ansi.Parser {
	p := &ansi.Parser{}
	p.SetParamsSize(32)
	// String payloads are ignored, so do not retain OSC/DCS data.
	p.SetDataSize(1)
	return p
}

func (b *Buffer) indexClean(input []byte) {
	if b.clean == nil {
		b.clean = &cleanIndex{parser: newCleanParser(), baseParser: newCleanParser(), checkpoints: []int64{0}}
	}
	c := b.clean
	futureBase := max(int64(0), b.total+int64(len(input))-int64(b.limit))
	last := int64(0)
	lineCount := b.totalLines
	if len(c.checkpoints) > 0 {
		last = c.checkpoints[len(c.checkpoints)-1]
	}
	for i, v := range input {
		c.parser.Advance(v)
		pos := b.total + int64(i+1)
		if v == '\n' {
			lineCount++
		}
		if pos%cleanCheckpointBytes == 0 && pos >= futureBase {
			c.lines = append(c.lines, lineCheckpoint{offset: pos, count: lineCount})
		}
		if pos-last >= cleanCheckpointBytes && c.parser.State() == parser.GroundState {
			if pos >= futureBase {
				c.checkpoints = append(c.checkpoints, pos)
			}
			last = pos
		}
	}
}

func (b *Buffer) cleanDropByte(v byte) {
	c := b.clean
	if c == nil {
		return
	}
	c.baseParser.Advance(v)
	if c.baseParser.State() == parser.GroundState {
		c.prefix = c.prefix[:0]
		c.prefixLost = false
		return
	}
	if len(c.prefix) < MaxCleanReplayBytes {
		c.prefix = append(c.prefix, v)
	} else {
		c.prefixLost = true
	}
}

func (b *Buffer) trimCleanIndex() {
	if b.clean == nil {
		return
	}
	base := b.total - int64(b.size)
	c := b.clean
	if b.size == 0 {
		c.checkpoints = nil
		c.lines = nil
		if len(c.prefix) == 0 {
			c.prefix = nil
		}
		return
	}
	n := sort.Search(len(c.checkpoints), func(i int) bool { return c.checkpoints[i] >= base })
	c.checkpoints = c.checkpoints[n:]
	n = sort.Search(len(c.lines), func(i int) bool { return c.lines[i].offset >= base })
	c.lines = c.lines[n:]
}

// lineAt uses an independent physical-line index, including inside ignored ANSI
// strings. Repeated pagination never rescans an increasing history prefix.
func (b *Buffer) lineAt(start int) int64 {
	base := b.total - int64(b.size)
	from, count := 0, b.droppedLines
	if b.clean != nil {
		index := b.clean.lines
		n := sort.Search(len(index), func(i int) bool { return index[i].offset > base+int64(start) })
		if n > 0 {
			from = int(index[n-1].offset - base)
			count = index[n-1].count
		}
	}
	for i := from; i < start; i++ {
		if b.at(i) == '\n' {
			count++
		}
	}
	return count + 1
}

// cleanRange renders only [start,end) source bytes. Preceding source restores
// lexical parser state, but contributes no presented text. Thus pages need not
// concatenate into a terminal screenshot: overwrites affect only their page.
// Work is bounded by the selected bytes plus MaxCleanReplayBytes, independent
// of capture history length. The current line cannot exceed selected bytes.
func (b *Buffer) cleanRange(start, end int) (string, error) {
	text, _, err := b.cleanRangeMapped(start, end, 0)
	return text, err
}

func (b *Buffer) cleanRangeMapped(start, end int, physicalLine int64) (string, []PresentedLine, error) {
	if start == end {
		return "", nil, nil
	}
	base := b.total - int64(b.size)
	c := b.clean
	p := newCleanParser()
	from := 0
	if c != nil {
		n := sort.Search(len(c.checkpoints), func(i int) bool { return c.checkpoints[i] > base+int64(start) })
		if n > 0 {
			from = int(c.checkpoints[n-1] - base)
		} else {
			if c.prefixLost || len(c.prefix)+start > MaxCleanReplayBytes {
				return "", nil, errCleanReplay
			}
			p.Parse(c.prefix)
		}
	}
	if start-from > MaxCleanReplayBytes {
		return "", nil, errCleanReplay
	}
	for i := from; i < start; i++ {
		p.Advance(b.at(i))
	}
	var out strings.Builder
	// Cells grow only to the page's high-water cursor position. Erasing a line
	// changes its generation instead of rematerializing an increasing blank
	// prefix on each subsequent print. Stale cells are blanks only at emission.
	cells := make([]cleanCell, 0, min(end-start, 4096))
	generation := uint64(1)
	extent := 0
	column := 0
	blankPrefix := 0 // Avoid repeatedly clearing the same long prefix for CSI 1 K.
	var lines []PresentedLine
	sourcePos, origin, originLine := base+int64(start), base+int64(start), physicalLine
	rawLineStart := sourcePos
	haveOrigin := false
	markOrigin := func() {
		if !haveOrigin {
			origin = rawLineStart
			originLine = physicalLine
			haveOrigin = true
		}
	}
	emit := func(newline bool) {
		var rendered strings.Builder
		for i := 0; i < extent; i++ {
			if cells[i].generation == generation {
				rendered.WriteRune(cells[i].r)
			} else {
				rendered.WriteByte(' ')
			}
		}
		text := rendered.String()
		if newline {
			text += "\n"
		}
		out.WriteString(text)
		if text != "" {
			lines = append(lines, PresentedLine{Text: text, Start: origin, Line: originLine})
		}
		extent = 0
		generation++
		column = 0
		blankPrefix = 0
		haveOrigin = false
	}
	printRune := func(r rune) {
		markOrigin()
		blankPrefix = min(blankPrefix, column)
		for len(cells) <= column {
			cells = append(cells, cleanCell{})
		}
		cells[column] = cleanCell{r: r, generation: generation}
		column++
		extent = max(extent, column)
	}
	p.SetHandler(ansi.Handler{
		Print: printRune,
		Execute: func(v byte) {
			switch v {
			case '\n':
				markOrigin()
				emit(true)
			case '\r':
				column = 0
			case '\b':
				column = max(0, column-1)
			case '\t':
				printRune('\t')
			default:
				printRune(rune(v))
			}
		},
		HandleCsi: func(cmd ansi.Cmd, params ansi.Params) {
			if cmd != ansi.Cmd('K') {
				return
			}
			mode, _, _ := params.Param(0, 0)
			switch mode {
			case 0:
				extent = min(column, extent)
				blankPrefix = min(blankPrefix, extent)
			case 1:
				end := min(column+1, extent)
				if end == extent {
					generation++
				} else {
					for i := blankPrefix; i < end; i++ {
						cells[i] = cleanCell{}
					}
				}
				blankPrefix = max(blankPrefix, end)
			case 2:
				extent = 0
				generation++
				blankPrefix = 0
			}
		},
	})
	invalidRun := false
	for i := start; i < end; i++ {
		sourcePos = base + int64(i)
		v := b.at(i)
		if p.State() == parser.GroundState {
			if v >= 0xa0 {
				var encoded [utf8.UTFMax]byte
				n := min(len(encoded), b.size-i)
				for j := range n {
					encoded[j] = b.at(i + j)
				}
				r, size := utf8.DecodeRune(encoded[:n])
				if r == utf8.RuneError && size == 1 {
					if !invalidRun {
						printRune('?')
					}
					invalidRun = true
					continue
				}
			}
		}
		invalidRun = false
		p.Advance(v)
		if v == '\n' {
			physicalLine++
			rawLineStart = sourcePos + 1
		}
	}
	emit(false)
	return out.String(), lines, nil
}

// ReadView selects the same source range as Read, optionally cleaning its model
// presentation. Read itself remains the lossless/raw capture API.
func (b *Buffer) ReadView(cursor string, limit int, stripANSI bool) (Page, error) {
	b.pool.mu.Lock()
	defer b.pool.mu.Unlock()
	page, err := b.read(cursor, limit)
	if err != nil || !stripANSI {
		return page, err
	}
	start := int(page.Start - (b.total - int64(b.size)))
	page.Output, page.Lines, err = b.cleanRangeMapped(start, start+len(page.Output), page.Line)
	return page, err
}

// StripEnabled gives nullable tool options a clean-by-default interpretation.
func StripEnabled(value *bool) bool { return value == nil || *value }
