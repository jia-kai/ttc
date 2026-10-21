package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"ttc/internal/capture"
	"ttc/internal/history"
	"ttc/internal/lsp"
	"ttc/internal/prompts"
)

// GrepOrigin records the raw source range searched once when a grep job is
// created. Byte bounds are absolute source-stream offsets, end-exclusive.
// Output cursors instead refer to the concatenated matching lines in stdout.
// Truncated reports source retention loss, including an initial partial line.
type GrepOrigin struct {
	SourceJobID   string `json:"source_job_id"`
	Stream        string `json:"stream"`
	Pattern       string `json:"pattern"`
	Literal       bool   `json:"literal"`
	CaseSensitive bool   `json:"case_sensitive"`
	StartByte     int64  `json:"start_byte"`
	EndByte       int64  `json:"end_byte"`
	Truncated     bool   `json:"truncated"`
	MatchCount    int64  `json:"match_count"`
}

// grep scans outside the manager lock. The owned registration joins the scan
// before runtime cleanup can release its capture pool. Only successful scans
// publish a completed job; no process, goroutine or notification is created.
func (m *Manager) grep(ctx context.Context, owner, id string, options ReadOptions) (map[string]any, error) {
	if options.Cursor != "" {
		return nil, errors.New(prompts.JobGrepCursor)
	}
	if options.Limit < 1 {
		return nil, errors.New(prompts.CapturePositiveReadLimit)
	}
	pattern := options.Grep
	if options.Literal {
		pattern = regexp.QuoteMeta(pattern)
	}
	if options.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	expression, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf(prompts.JobOutputGrepFailure, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	if !m.RegisterOwned(owner, cancel, done) {
		cancel()
		return nil, errors.New(prompts.RuntimeEnded)
	}
	defer func() { cancel(); close(done); m.ReleaseOwned(owner, done) }()
	if err := m.acquireGrepScan(ctx); err != nil {
		return nil, err
	}
	defer func() { <-m.grepScans }()
	m.mu.Lock()
	source, ok := m.jobs[id]
	if !ok || !allowed(owner, source.view.Owner) {
		m.mu.Unlock()
		return nil, ErrNotFound
	}
	stream := options.Stream
	if stream == "" {
		stream = "stdout"
		if source.client != nil {
			stream = "stderr"
		}
	}
	if source.client != nil && stream != "stderr" {
		m.mu.Unlock()
		return nil, &lsp.Error{Code: "invalid_input", Message: prompts.JobLSPProtocolStdout}
	}
	buffer := source.stdout
	if stream == "stderr" {
		buffer = source.stderr
	} else if stream != "stdout" {
		m.mu.Unlock()
		return nil, errors.New(prompts.JobInvalidStream)
	}
	m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Each stream retains at most CallLimit/2 bytes. Read acquires one raw
	// snapshot; neither source writes nor subsequent pool eviction can alter it.
	page, err := buffer.Read("", capture.CallLimit/2)
	if err != nil {
		return nil, err
	}
	origin := &GrepOrigin{SourceJobID: id, Stream: stream, Pattern: options.Grep, Literal: options.Literal, CaseSensitive: !options.IgnoreCase, StartByte: page.Start, EndByte: page.End, Truncated: page.Truncated}
	out := m.pool.NewBuffer(capture.CallLimit / 2)
	keep := false
	defer func() {
		if !keep {
			out.Release()
		}
	}()
	origin.MatchCount, err = scanGrep(ctx, page.Output, expression, out)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	closed := make(chan struct{})
	close(closed)
	j := &job{startedAt: now, view: Snapshot{ID: history.NewID("job"), Kind: "grep", Owner: owner, Label: "grep", Status: "completed", StartedAt: now.Format(time.RFC3339Nano), FinishedAt: now.Format(time.RFC3339Nano), Grep: origin}, cancel: func() {}, done: closed, published: closed, stdout: out, inspector: grepInspector}
	m.mu.Lock()
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New(prompts.RuntimeEnded)
	}
	j.stderr = m.pool.NewBuffer(capture.CallLimit / 2)
	m.jobs[j.view.ID] = j
	keep = true
	view := cloneSnapshot(j.view)
	m.mu.Unlock()
	if m.OnState != nil {
		m.OnState(view)
	}
	return m.Read(ctx, owner, j.view.ID, ReadOptions{Limit: options.Limit, StripANSI: options.StripANSI})
}

// acquireGrepScan admits at most two temporary source snapshots per manager.
// Owned registration must precede admission so owner shutdown joins waiters.
func (m *Manager) acquireGrepScan(ctx context.Context) error {
	select {
	case m.grepScans <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-m.ctx.Done():
		return m.ctx.Err()
	}
}

const grepChunkBytes = 64 << 10

// scanGrep matches complete raw physical lines, writing exact matches in bounded
// chunks. Both many tiny lines and a single large line require O(bytes/chunk)
// output writes, rather than one pool-lock acquisition per matching line.
func scanGrep(ctx context.Context, raw string, expression *regexp.Regexp, out io.Writer) (int64, error) {
	chunk := make([]byte, 0, grepChunkBytes)
	flush := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := out.Write(chunk)
		if err == nil && n != len(chunk) {
			err = io.ErrShortWrite
		}
		chunk = chunk[:0]
		return err
	}
	var count int64
	for remaining := raw; remaining != ""; {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		n := strings.IndexByte(remaining, '\n')
		if n < 0 {
			n = len(remaining)
		} else {
			n++
		}
		line := remaining[:n]
		if expression.MatchString(strings.TrimSuffix(line, "\n")) {
			count++
			for line != "" {
				take := min(len(line), cap(chunk)-len(chunk))
				chunk = append(chunk, line[:take]...)
				line = line[take:]
				if len(chunk) == cap(chunk) {
					if err := flush(); err != nil {
						return count, err
					}
				}
			}
		}
		remaining = remaining[n:]
	}
	if len(chunk) > 0 {
		if err := flush(); err != nil {
			return count, err
		}
	}
	return count, ctx.Err()
}

func grepInspector(view Snapshot) InspectionSpec {
	o := view.Grep
	return InspectionSpec{Fields: []InspectionField{{Name: "Source job", Value: o.SourceJobID}, {Name: "Source stream", Value: o.Stream}, {Name: "Filter", Value: fmt.Sprintf("%s (literal=%t, case_sensitive=%t)", o.Pattern, o.Literal, o.CaseSensitive)}, {Name: "Source snapshot", Value: fmt.Sprintf("bytes [%d,%d), truncated=%t, matches=%d", o.StartByte, o.EndByte, o.Truncated, o.MatchCount)}}, Streams: defaultInspectionStreams(false)[:1]}
}
