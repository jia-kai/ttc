package capture

import (
	"errors"
	"fmt"
	"strings"

	"ttc/internal/prompts"
)

// OutputOptions controls a combined stdout/stderr payload byte budget. Zero
// values select head_tail and 8192 bytes. The hard ceiling is 8192 source bytes.
// StripANSI defaults to true; cleaning does not replenish the source budget.
type OutputOptions struct {
	Mode      string
	Bytes     int
	StripANSI *bool // Nil cleans model presentation; false preserves raw output.
}

const (
	// DefaultOutputBytes is the modest combined stream payload budget.
	DefaultOutputBytes = 8192
	// MaxOutputBytes leaves room beneath the 64-KiB tool-result limit even when
	// JSON expands every source byte into a six-byte escape. It does not bound
	// unrelated metadata such as an arbitrarily long command label.
	MaxOutputBytes = 8192
)

// Normalize supplies defaults and rejects invalid selection modes or budgets.
func (o OutputOptions) Normalize() (OutputOptions, error) {
	if o.StripANSI != nil {
		value := *o.StripANSI
		o.StripANSI = &value
	}
	if o.Mode == "" {
		o.Mode = "head_tail"
	}
	if o.Bytes == 0 {
		o.Bytes = DefaultOutputBytes
	}
	if o.Mode != "head" && o.Mode != "tail" && o.Mode != "head_tail" {
		return o, errors.New(prompts.ToolShellOutputMode)
	}
	if o.Bytes < 1 || o.Bytes > MaxOutputBytes {
		return o, fmt.Errorf(prompts.ToolIntegerRange, "output_bytes", 1, MaxOutputBytes)
	}
	return o, nil
}

// Selection describes a stream payload consisting of its head followed by its
// tail. Segment lengths are source bytes; OmittedBytes includes lost retention.
// Cleaned Output may be shorter than HeadBytes+TailBytes. A CleaningError marks
// a segment whose bounded lexical context could not be recovered; raw remains
// available by selecting StripANSI=false.
// OmittedBytes minus LostPrefixBytes is retained data skipped by selection.
// LostPrefixBytes cannot be recovered by paging. Head starts at the oldest
// retained byte, not necessarily byte zero. No separator is inserted in Output.
type Selection struct {
	Output          string `json:"-"`
	TotalBytes      int64  `json:"total_bytes"`
	OmittedBytes    int64  `json:"omitted_bytes"`
	LostPrefixBytes int64  `json:"lost_prefix_bytes"`
	HeadBytes       int    `json:"head_bytes"`
	TailBytes       int    `json:"tail_bytes"`
	CleaningError   string `json:"cleaning_error,omitempty"` // Explicit bounded-context failure; raw capture remains readable.
}

// SelectOutput snapshots separate streams under their shared pool lock, using
// one combined budget. Each gets half when both are large; unused space goes
// to the other stream. Complete retained output is returned whenever it fits.
// Valid UTF-8 runes are never split; invalid byte runs become question marks.
func SelectOutput(stdout, stderr *Buffer, options OutputOptions) (Selection, Selection) {
	if stdout.pool != stderr.pool {
		panic("output streams must share a capture pool")
	}
	options, err := options.Normalize()
	if err != nil {
		panic(err)
	}
	p := stdout.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	a, b := min(stdout.size, (options.Bytes+1)/2), min(stderr.size, options.Bytes/2)
	spare := options.Bytes - a - b
	extra := min(spare, stdout.size-a)
	a += extra
	b += min(spare-extra, stderr.size-b)
	x, y := stdout.selectOutput(options.Mode, a), stderr.selectOutput(options.Mode, b)
	if StripEnabled(options.StripANSI) {
		stdout.cleanSelection(&x)
		stderr.cleanSelection(&y)
	}
	return x, y
}

func (b *Buffer) cleanSelection(s *Selection) {
	head, err := b.cleanRange(0, s.HeadBytes)
	if err != nil {
		s.Output = ""
		s.CleaningError = err.Error()
		return
	}
	tail, err := b.cleanRange(b.size-s.TailBytes, b.size)
	if err != nil {
		s.Output = head
		s.CleaningError = err.Error()
		return
	}
	s.Output = head + tail
}

func (b *Buffer) selectOutput(mode string, budget int) Selection {
	s := Selection{TotalBytes: b.total, LostPrefixBytes: b.total - int64(b.size)}
	if b.size <= budget {
		s.HeadBytes = b.size
	} else {
		switch mode {
		case "head":
			s.HeadBytes = b.boundary(budget, false)
		case "tail":
			s.TailBytes = b.size - b.boundary(b.size-budget, true)
		case "head_tail":
			s.HeadBytes = b.boundary((budget+1)/2, false)
			s.TailBytes = b.size - b.boundary(b.size-budget/2, true)
		}
	}
	data := make([]byte, s.HeadBytes+s.TailBytes)
	for i := 0; i < s.HeadBytes; i++ {
		data[i] = b.at(i)
	}
	for i := 0; i < s.TailBytes; i++ {
		data[s.HeadBytes+i] = b.at(b.size - s.TailBytes + i)
	}
	s.Output = strings.ToValidUTF8(string(data), "?")
	s.OmittedBytes = b.total - int64(len(data))
	return s
}
