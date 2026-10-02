package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gdamore/tcell/v2"
	"golang.org/x/sys/unix"
	"scicode/internal/render"
)

type completionQuery struct {
	root, text, prefix string
	cursor, start, end int
	generation         uint64
	marker             rune
}
type completionItem struct {
	value, description string
	directory          bool
}
type completionResult struct {
	query     completionQuery
	items     []completionItem
	err       error
	truncated bool
}
type completionMenu struct {
	result       completionResult
	selected     int
	dismissed    completionQuery
	hasDismissed bool
}

var slashCommands = []completionItem{
	{value: "/btw", description: "Parallel read-only side question"},
	{value: "/background", description: "Move foreground shells to background"},
	{value: "/editor", description: "Edit input in an external editor"},
	{value: "/history", description: "Browse history branches"}, {value: "/branch", description: "Restore an explicit history entry"},
	{value: "/help", description: "Keyboard and command guide"}, {value: "/new", description: "Start a session"}, {value: "/clear", description: "Start a session"},
	{value: "/sessions", description: "List sessions"}, {value: "/load", description: "Open a session"}, {value: "/undo", description: "Undo file edits"}, {value: "/redo", description: "Redo file edits"},
	{value: "/rename", description: "Rename the current session"},
	{value: "/export", description: "Export Markdown and exact JSONL"}, {value: "/compact", description: "Summarize earlier context"}, {value: "/jobs", description: "Inspect jobs"}, {value: "/timers", description: "Inspect timers"},
	{value: "/inspect", description: "Inspect an entry"}, {value: "/attach", description: "Snapshot an attachment"}, {value: "/model", description: "Choose model"}, {value: "/models", description: "Choose model"},
	{value: "/questions", description: "Open pending questions"}, {value: "/answer", description: "Answer in plain mode"}, {value: "/login", description: "Device login"}, {value: "/quit", description: "Exit"},
}

// completionAt only recognizes commands at the beginning and @ at a word
// boundary, avoiding email addresses. Offsets are rune indices into the draft.
func completionAt(c composer, root string, generation uint64) (completionQuery, bool) {
	r := []rune(c.text)
	if c.pasting || c.cursor < 1 {
		return completionQuery{}, false
	}
	start := -1
	quoted := false
	for i := 0; i < c.cursor; i++ {
		if quoted {
			if r[i] == '\\' && i+1 < c.cursor {
				i++
				continue
			}
			if r[i] == '"' {
				start = -1
				quoted = false
			}
			continue
		}
		if unicode.IsSpace(r[i]) {
			start = -1
			continue
		}
		if r[i] == '@' && (i == 0 || unicode.IsSpace(r[i-1])) {
			start = i
			if i+1 < c.cursor && r[i+1] == '"' {
				quoted = true
				i++
			}
		} else if r[i] == '/' && i == 0 {
			start = 0
		}
	}
	if start < 0 {
		return completionQuery{}, false
	}
	prefixStart := start + 1
	end := c.cursor
	if quoted {
		prefixStart++
		for end < len(r) && r[end] != '"' {
			end++
		}
		if end < len(r) {
			end++
		}
	} else {
		for end < len(r) && !unicode.IsSpace(r[end]) {
			end++
		}
	}
	prefix := string(r[prefixStart:c.cursor])
	if quoted {
		decoded, err := strconv.Unquote("\"" + prefix + "\"")
		if err != nil {
			return completionQuery{}, false
		}
		prefix = decoded
	}
	return completionQuery{root: root, text: c.text, cursor: c.cursor, start: start, end: end, prefix: prefix, marker: r[start], generation: generation}, true
}

func commandCompletions(q completionQuery) completionResult {
	out := completionResult{query: q}
	for _, item := range slashCommands {
		if strings.HasPrefix(item.value, "/"+q.prefix) {
			out.items = append(out.items, item)
		}
	}
	return out
}

// listPaths reads one directory incrementally, bounding both matches and work.
// It never indexes a workspace recursively or performs I/O on the UI goroutine.
func listPaths(ctx context.Context, q completionQuery) completionResult {
	out := completionResult{query: q}
	dir, prefix := filepath.Split(q.prefix)
	path := filepath.Join(q.root, dir)
	if filepath.IsAbs(q.prefix) {
		path = dir
	}
	if err := ctx.Err(); err != nil {
		out.err = err
		return out
	}
	// Nonblocking open prevents a mistyped FIFO path from stopping the worker
	// before directory validation or cancellation can run.
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		out.err = err
		return out
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		out.err = err
		return out
	}
	if !info.IsDir() {
		out.err = fmt.Errorf("path completion requires a directory: %s", path)
		return out
	}
	// "@./" can snapshot a directory itself; other directory rows navigate.
	if prefix == "" {
		out.items = append(out.items, completionItem{value: dir + ".", description: "Attach this directory"})
	}
	scanned := 0
	for scanned < 10000 && len(out.items) < 256 {
		if err := ctx.Err(); err != nil {
			out.err = err
			return out
		}
		entries, err := f.ReadDir(min(128, 10000-scanned))
		scanned += len(entries)
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), prefix) {
				continue
			}
			item := completionItem{value: dir + entry.Name(), directory: entry.IsDir()}
			if item.directory {
				item.value += "/"
			}
			out.items = append(out.items, item)
			if len(out.items) == 256 {
				break
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			out.err = err
			return out
		}
	}
	out.truncated = scanned >= 10000 || len(out.items) >= 256
	sort.Slice(out.items, func(i, j int) bool { return out.items[i].value < out.items[j].value })
	return out
}

type pathCompleter struct {
	mu           sync.Mutex
	activeCancel context.CancelFunc
	queries      chan completionQuery
	results      chan completionResult
	cancel       context.CancelFunc
	done         chan struct{}
}

func newPathCompleter(ctx context.Context) *pathCompleter {
	ctx, cancel := context.WithCancel(ctx)
	p := &pathCompleter{queries: make(chan completionQuery, 1), results: make(chan completionResult, 1), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		for {
			var q completionQuery
			select {
			case <-ctx.Done():
				return
			case q = <-p.queries:
			}
			timer := time.NewTimer(25 * time.Millisecond)
		debounce:
			for {
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case q = <-p.queries:
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(25 * time.Millisecond)
				case <-timer.C:
					break debounce
				}
			}
			select {
			case newer := <-p.queries:
				q = newer
			default:
			}
			jobCtx, cancelJob := context.WithCancel(ctx)
			p.mu.Lock()
			p.activeCancel = cancelJob
			p.mu.Unlock()
			result := listPaths(jobCtx, q)
			cancelJob()
			p.mu.Lock()
			p.activeCancel = nil
			p.mu.Unlock()
			select {
			case <-p.results:
			default:
			}
			select {
			case p.results <- result:
			case <-ctx.Done():
				return
			}
		}
	}()
	return p
}
func (p *pathCompleter) query(q completionQuery) {
	p.mu.Lock()
	if p.activeCancel != nil {
		p.activeCancel()
	}
	p.mu.Unlock()
	select {
	case <-p.queries:
	default:
	}
	p.queries <- q
}
func (p *pathCompleter) close() { p.cancel(); <-p.done }

func (m *completionMenu) draw(s tcell.Screen, inputY int) {
	w, _ := s.Size()
	count := min(7, min(len(m.result.items), max(0, inputY-1)))
	if count == 0 {
		return
	}
	first := max(0, m.selected-count+1)
	style := tcell.StyleDefault.Background(tcell.GetColor(render.SurfaceColor)).Foreground(tcell.GetColor(render.TextColor))
	for i := 0; i < count; i++ {
		item := m.result.items[first+i]
		rowStyle := style
		if first+i == m.selected {
			rowStyle = rowStyle.Foreground(tcell.GetColor(render.CyanColor)).Reverse(true)
		}
		put(s, 0, inputY-count+i, w, strings.Repeat(" ", w), rowStyle)
		put(s, 1, inputY-count+i, max(0, w-2), render.Clean(fmt.Sprintf("%s  %s", item.value, item.description)), rowStyle)
	}
}

func (m *completionMenu) accept(c *composer) (path string, accepted bool) {
	if m.selected >= len(m.result.items) {
		return "", false
	}
	item, q := m.result.items[m.selected], m.result.query
	r := []rune(c.text)
	value := item.value
	if q.marker == '@' {
		value = "@" + value
		if strings.ContainsAny(item.value, " \t\n\"") {
			value = "@" + strconv.Quote(item.value)
		}
		if !item.directory {
			path = item.value
			value += " "
		}
	}
	c.text = string(r[:q.start]) + value + string(r[q.end:])
	c.cursor = q.start + len([]rune(value))
	if item.directory && strings.HasSuffix(value, "\"") {
		c.cursor--
	}
	return path, true
}
