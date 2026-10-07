// Package capture owns bounded output rings shared by one active runtime.
package capture

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"ttc/internal/prompts"
)

// CallLimit and SharedLimit are the default retained payload limits, in bytes.
const (
	CallLimit   = 64 << 20
	SharedLimit = 1 << 30
)

// Pool bounds the allocated capacity of all its output rings. When full it evicts
// the least recently written ring; readers see the loss through Truncated.
type Pool struct {
	mu          sync.Mutex
	limit, used int
	clock       uint64
	buffers     []*Buffer
}

// NewPool creates a runtime owner. Limits must leave room for two stream rings.
func NewPool(limit int) *Pool {
	if limit < 2 {
		panic("capture pool limit must be at least two bytes")
	}
	return &Pool{limit: limit}
}

// Buffer is an io.Writer retaining the newest bytes. Its pool owns synchronization.
type Buffer struct {
	pool                *Pool
	limit               int
	data                []byte
	start, size         int
	total, droppedLines int64
	totalLines          int64
	last                uint64
	truncated           bool
	previewEnd          int64 // Absolute end of the last nonblank line; includes its terminating newline.
	previewLineContent  bool  // Whether the current physical line has non-whitespace bytes.
}

// NewBuffer allocates no payload until the first write. limit is in bytes and is
// capped at half the shared pool to allow a ring to grow without exceeding it.
func (p *Pool) NewBuffer(limit int) *Buffer {
	p.mu.Lock()
	defer p.mu.Unlock()
	if limit < 1 {
		panic("capture buffer limit must be positive")
	}
	b := &Buffer{pool: p, limit: min(limit, p.limit/2)}
	p.buffers = append(p.buffers, b)
	return b
}

func (b *Buffer) at(i int) byte {
	return b.data[(b.start+i)%len(b.data)]
}

// boundary adjusts only positions inside valid runes. Invalid source bytes are
// retained verbatim so absolute offsets remain meaningful for binary output.
func (b *Buffer) boundary(pos int, forward bool) int {
	if pos == 0 || pos == b.size {
		return pos
	}
	for start := max(0, pos-3); start < pos; start++ {
		var encoded [utf8.UTFMax]byte
		n := min(len(encoded), b.size-start)
		for i := range n {
			encoded[i] = b.at(start + i)
		}
		_, size := utf8.DecodeRune(encoded[:n])
		if size > 1 && start+size > pos {
			if forward {
				return start + size
			}
			return start
		}
	}
	return pos
}

func (b *Buffer) drop(n int) {
	for i := 0; i < n; i++ {
		if b.data[(b.start+i)%len(b.data)] == '\n' {
			b.droppedLines++
		}
	}
	if n > 0 {
		b.start = (b.start + n) % len(b.data)
		b.size -= n
		b.truncated = true
	}
}

// Write always accepts the entire input, dropping older data as necessary.
func (b *Buffer) Write(input []byte) (int, error) {
	p := b.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(input)
	if n == 0 {
		return 0, nil
	}
	b.totalLines += int64(bytes.Count(input, []byte{'\n'}))
	for i, char := range input {
		if char == '\n' {
			if b.previewLineContent {
				b.previewEnd = b.total + int64(i+1)
			}
			b.previewLineContent = false
		} else {
			if char != ' ' && char != '\t' && char != '\r' && char != '\v' && char != '\f' {
				b.previewLineContent = true
			}
			if b.previewLineContent {
				b.previewEnd = b.total + int64(i+1)
			}
		}
	}
	p.clock++
	b.last = p.clock
	if len(input) > b.limit {
		b.drop(b.size)
		omitted := len(input) - b.limit
		b.droppedLines += int64(bytes.Count(input[:omitted], []byte{'\n'}))
		input = input[omitted:]
		b.truncated = true
	}
	desired := min(b.limit, b.size+len(input))
	if desired > len(b.data) {
		capacity := min(b.limit, max(desired, max(4096, len(b.data)*2)))
		for p.used+capacity > p.limit {
			var victim *Buffer
			for _, candidate := range p.buffers {
				if candidate != b && len(candidate.data) > 0 && (victim == nil || candidate.last < victim.last) {
					victim = candidate
				}
			}
			if victim == nil {
				panic("capture capacity invariant")
			}
			victim.drop(victim.size)
			p.used -= len(victim.data)
			victim.data = nil
			victim.start = 0
		}
		data := make([]byte, capacity)
		if b.size > 0 {
			first := copy(data, b.data[b.start:min(len(b.data), b.start+b.size)])
			copy(data[first:], b.data[:b.size-first])
		}
		p.used += capacity - len(b.data)
		b.data = data
		b.start = 0
	}
	if b.size+len(input) > b.limit {
		b.drop(b.size + len(input) - b.limit)
	}
	end := (b.start + b.size) % len(b.data)
	first := copy(b.data[end:], input)
	copy(b.data, input[first:])
	b.size += len(input)
	b.total += int64(n)
	return n, nil
}

// Tail skips trailing ASCII-whitespace-only lines, then returns at most the
// newest lines and bytes without splitting UTF-8. Read preserves the raw bytes.
func (b *Buffer) Tail(lines, limit int) string {
	if lines <= 0 || limit <= 0 {
		return ""
	}
	p := b.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	end := int(max(0, b.previewEnd-(b.total-int64(b.size))))
	data := make([]byte, min(limit, end))
	for i := range data {
		data[i] = b.data[(b.start+end-len(data)+i)%len(b.data)]
	}
	start := tailLineStart(data, lines)
	for start < len(data) && !utf8.RuneStart(data[start]) {
		start++
	}
	return strings.ToValidUTF8(string(data[start:]), "?")
}

func tailLineStart(data []byte, lines int) int {
	end := len(data)
	if end > 0 && data[end-1] == '\n' {
		end--
	}
	for i := end - 1; i >= 0; i-- {
		if data[i] == '\n' {
			lines--
			if lines == 0 {
				return i + 1
			}
		}
	}
	return 0
}

// Page is a retained byte range. Output preserves source bytes, including invalid
// UTF-8; JSON presentation replaces invalid bytes. End is the observed EOF offset,
// and Line is the one-based physical line at Start.
type Page struct {
	Output, NextCursor string
	Start, End, Line   int64
	Truncated          bool
}

// Read accepts an absolute byte cursor or eof:-N:bytes / eof:-N:lines. Empty starts
// at the oldest retained byte. EOF is resolved from one locked snapshot. Lost
// prefixes clamp to the retained range; cursor beyond EOF is an error.
func (b *Buffer) Read(cursor string, limit int) (Page, error) {
	p := b.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if limit < 1 {
		return Page{}, errors.New(prompts.CapturePositiveReadLimit)
	}
	if cursor == "eof:0:bytes" || cursor == "eof:0:lines" {
		return Page{NextCursor: fmt.Sprint(b.total), Start: b.total, End: b.total, Line: b.totalLines + 1, Truncated: b.truncated}, nil
	}
	base := b.total - int64(b.size)
	pos := base
	lost := b.truncated
	if strings.HasPrefix(cursor, "eof:") {
		parts := strings.Split(cursor, ":")
		if len(parts) != 3 {
			return Page{}, errors.New(prompts.CaptureEOFCursorFormat)
		}
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || n > 0 || n == (-1<<63) {
			return Page{}, errors.New(prompts.CaptureEOFOffset)
		}
		switch parts[2] {
		case "bytes":
			pos = b.total + n
		case "lines":
			if n == 0 {
				pos = b.total
			} else {
				end := b.size
				if end > 0 && b.at(end-1) == '\n' {
					end--
				}
				left, start := -n, 0
				for i := end - 1; i >= 0; i-- {
					if b.at(i) == '\n' {
						left--
						if left == 0 {
							start = i + 1
							break
						}
					}
				}
				pos = base + int64(start)
			}
		default:
			return Page{}, errors.New(prompts.CaptureEOFUnit)
		}
	} else if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n < 0 {
			return Page{}, errors.New(prompts.CaptureAbsoluteCursor)
		}
		pos = n
	}
	if pos < base {
		pos = base
		lost = true
	}
	if pos > b.total {
		return Page{}, errors.New(prompts.CaptureCursorBeyondOutput)
	}
	start := int(pos - base)
	start = b.boundary(start, true)
	end := b.boundary(min(b.size, start+limit), false)
	if start < b.size && end == start {
		return Page{}, errors.New(prompts.CaptureUTF8ReadLimit)
	}
	data := make([]byte, end-start)
	for i := range data {
		data[i] = b.at(start + i)
	}
	line := b.droppedLines + 1
	if start < b.size/2 {
		for i := 0; i < start; i++ {
			if b.at(i) == '\n' {
				line++
			}
		}
	} else {
		line = b.totalLines + 1
		for i := start; i < b.size; i++ {
			if b.at(i) == '\n' {
				line--
			}
		}
	}
	return Page{Output: string(data), NextCursor: fmt.Sprint(base + int64(end)), Start: base + int64(start), End: b.total, Line: line, Truncated: lost}, nil
}

// Clear releases every retained ring once its runtime has joined all writers.
func (p *Pool) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.buffers {
		b.drop(b.size)
		b.data = nil
		b.start = 0
	}
	p.used = 0
	p.buffers = nil
}

// Stats reports retained allocation and configured shared limit, in bytes.
func (p *Pool) Stats() (used, limit int) { p.mu.Lock(); defer p.mu.Unlock(); return p.used, p.limit }
