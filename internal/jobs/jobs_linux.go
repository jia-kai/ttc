// Package jobs supervises bounded, session-local Linux shell process groups.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"ttc/internal/capture"
	"ttc/internal/prompts"

	"ttc/internal/history"
	"ttc/internal/lsp"
)

// ErrNotFound identifies a stale or inaccessible job handle without exposing
// another actor's jobs. Callers can map it to their own error protocol.
var ErrNotFound = errors.New("unknown or inaccessible job_id; use job_list(state=all) to choose an accessible job")

// ForegroundTimeout is the default deadline for foreground shells, in duration units.
// Foreground commands cannot run without a deadline; background shells default to none.
const ForegroundTimeout = 20 * time.Second

// Snapshot is an immutable job view. ExitCode is nil until an exit is observed.
type Snapshot struct {
	WakeOnExit bool   `json:"-"` // Whether the background completion should notify the model.
	ID         string `json:"job_id"`
	Kind       string `json:"kind"`
	Owner      string `json:"owner_actor_id"`
	Label      string `json:"label"`
	Status     string `json:"status"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	StartedAt  string `json:"started_at"` // UTC RFC3339Nano launch time.
	FinishedAt string `json:"finished_at,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Signal     string `json:"signal,omitempty"`
	Truncated  bool   `json:"truncated"`
}
type job struct {
	view             Snapshot
	startedAt        time.Time // Immutable launch time; ordering never depends on capture updates.
	cmd              *exec.Cmd
	cancel           context.CancelFunc
	done             chan struct{}
	promoted         chan struct{} // Closed once when foreground shell ownership moves to background.
	promotionClaimed bool          // The original waiter owns promotion; later waits observe ordinary completion.
	stdout, stderr   *capture.Buffer
	background       bool
	client           *lsp.Client
}

// Manager owns jobs for one runtime. Close cancels and joins all process groups.
type Manager struct {
	wg     sync.WaitGroup
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	jobs   map[string]*job
	closed bool
	Notify func(Snapshot)
	// OnState publishes launch and terminal states outside the manager lock.
	// Set before launching jobs; callbacks must not wait for this job to finish.
	OnState func(Snapshot)
	pool    *capture.Pool
}

// New constructs a live supervisor; foreground contexts do not own background jobs.
func New(ctx context.Context, notify func(Snapshot)) *Manager {
	ctx, cancel := context.WithCancel(ctx)
	return &Manager{ctx: ctx, cancel: cancel, jobs: map[string]*job{}, Notify: notify, pool: capture.NewPool(capture.SharedLimit)}
}

// Start launches a closed-stdin shell in a dedicated process group.
// strict enables POSIX errexit and nounset; it does not enable pipefail.
// Zero timeout selects ForegroundTimeout for foreground work and no deadline
// for background work. A positive timeout is an explicit deadline in either mode.
func (m *Manager) Start(owner, command, workdir string, timeout time.Duration, strict, background, wake bool) (string, error) {
	return m.start(owner, command, workdir, timeout, strict, background, wake, false)
}

// StartLSP launches a background stdio server. Protocol stdout is owned by the
// LSP client; only stderr is available through Read. Stop cancels the process group.
func (m *Manager) StartLSP(owner, command, workdir string, timeout time.Duration, strict, wake bool) (string, error) {
	return m.start(owner, command, workdir, timeout, strict, true, wake, true)
}

func (m *Manager) start(owner, command, workdir string, timeout time.Duration, strict, background, wake, protocol bool) (string, error) {
	if timeout < 0 {
		return "", errors.New("shell timeout must be nonnegative")
	}
	if !background && timeout == 0 {
		timeout = ForegroundTimeout
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", errors.New("runtime ended")
	}
	ctx, cancel := context.WithCancel(m.ctx)
	if timeout > 0 {
		cancel()
		ctx, cancel = context.WithTimeout(m.ctx, timeout)
	}
	args := []string{"-c", command}
	if strict {
		args = []string{"-eu", "-c", command}
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", args...)
	cmd.Dir = workdir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		e := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if e == syscall.ESRCH {
			return nil
		}
		return e
	}
	cmd.WaitDelay = time.Second
	startedAt := time.Now().UTC()
	j := &job{startedAt: startedAt, view: Snapshot{ID: history.NewID("job"), Kind: "shell", Owner: owner, Label: command, Status: "running", StartedAt: startedAt.Format(time.RFC3339Nano), WakeOnExit: wake}, cmd: cmd, cancel: cancel, done: make(chan struct{}), promoted: make(chan struct{}), background: background, stdout: m.pool.NewBuffer(capture.CallLimit / 2), stderr: m.pool.NewBuffer(capture.CallLimit / 2)}
	cmd.Stdout = j.stdout
	cmd.Stderr = j.stderr
	var input io.WriteCloser
	var output, writer *os.File
	if protocol {
		var err error
		input, err = cmd.StdinPipe()
		if err == nil {
			output, writer, err = os.Pipe()
		}
		if err != nil {
			if input != nil {
				_ = input.Close()
			}
			cancel()
			m.mu.Unlock()
			return "", fmt.Errorf("create LSP pipes: %w", err)
		}
		cmd.Stdout = writer
		j.view.Kind = "lsp"
	}
	if e := cmd.Start(); e != nil {
		if input != nil {
			_ = input.Close()
			_ = output.Close()
			_ = writer.Close()
		}
		cancel()
		m.mu.Unlock()
		return "", e
	}
	if protocol {
		_ = writer.Close() // Only the child retains the stdout writer.
		j.client = lsp.New(ctx, workdir, input, output)
	}
	m.jobs[j.view.ID] = j
	m.wg.Add(1)
	launch := j.view
	m.mu.Unlock()
	if m.OnState != nil {
		m.OnState(launch)
	}
	go func() {
		defer m.wg.Done()
		e := cmd.Wait() // Kill lingering descendants even if the group leader exits first.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if j.client != nil {
			j.client.Close()
		}
		m.mu.Lock()
		j.view.Status = "completed"
		if ctx.Err() != nil {
			j.view.Status = "cancelled"
		} else if e != nil {
			var exit *exec.ExitError
			if !errors.As(e, &exit) {
				j.view.Status = "failed"
				fmt.Fprintf(j.stderr, "Shell supervision failed: %v\n", e)
				if errors.Is(e, exec.ErrWaitDelay) {
					io.WriteString(j.stderr, prompts.ToolNote("shell", "descendant_pipes"))
				}
			}
		}
		if cmd.ProcessState != nil {
			code := cmd.ProcessState.ExitCode()
			if code >= 0 {
				j.view.ExitCode = &code
			} else if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
				j.view.Signal = ws.Signal().String()
			}
		}
		j.view.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		view := preview(j)
		view.WakeOnExit = wake
		notify := j.background
		m.mu.Unlock()
		if m.OnState != nil {
			m.OnState(view)
		}
		close(j.done)
		cancel()
		if notify && m.Notify != nil {
			m.Notify(view)
		}
	}()
	return launch.ID, nil
}

// QueryLSP performs a bounded read-only query on an actor-visible running server.
// The caller owns the query deadline; cancellation preserves the background job.
func (m *Manager) QueryLSP(ctx context.Context, owner, id string, query lsp.Query) (map[string]any, error) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok || !allowed(owner, j.view.Owner) {
		m.mu.Unlock()
		return nil, &lsp.Error{Code: "not_found", Message: "unknown or inaccessible live LSP job; use job_list or start shell(protocol=lsp, background=true)"}
	}
	if j.client == nil {
		m.mu.Unlock()
		return nil, &lsp.Error{Code: "invalid_input", Message: "job is not an LSP server; start shell(protocol=lsp, background=true)"}
	}
	if j.view.Status != "running" {
		m.mu.Unlock()
		return nil, &lsp.Error{Code: "job_not_running", Message: "language server exited; inspect job_read(stream=stderr) and start a new server"}
	}
	client := j.client
	m.mu.Unlock()
	return client.Query(ctx, query)
}
func allowed(owner, jobOwner string) bool {
	return owner == "main" || owner == jobOwner || strings.HasPrefix(jobOwner, owner+"/")
}

// View rejects stale or unrelated handles and returns a copy.
func (m *Manager) View(owner, id string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok || !allowed(owner, j.view.Owner) {
		return Snapshot{}, ErrNotFound
	}
	return preview(j), nil
}

// Wait waits for observed exit; cancellation stops foreground work only.
func (m *Manager) Wait(ctx context.Context, owner, id string, update func(Snapshot)) (Snapshot, error) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok || !allowed(owner, j.view.Owner) {
		m.mu.Unlock()
		return Snapshot{}, ErrNotFound
	}
	done := j.done
	var promoted <-chan struct{}
	if !j.promotionClaimed {
		promoted = j.promoted
		j.promotionClaimed = true
	}
	m.mu.Unlock()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if update != nil {
			view, err := m.View(owner, id)
			if err != nil {
				return Snapshot{}, err
			}
			if view.Status == "running" {
				update(view)
			}
		}
		select {
		case <-done:
			return m.View(owner, id)
		case <-promoted:
			view, err := m.View(owner, id)
			if err != nil || view.Status == "running" {
				return view, err
			}
			// Terminal state is observable before its durable publication. A
			// promoted waiter must join that commit before returning the result.
			<-done
			return m.View(owner, id)
		case <-ctx.Done():
			m.mu.Lock()
			if j.background {
				view := preview(j)
				m.mu.Unlock()
				if view.Status != "running" {
					<-done
					return m.View(owner, id)
				}
				return view, nil
			}
			j.cancel()
			m.mu.Unlock()
			<-done
			return m.View(owner, id)
		case <-ticker.C:
		}
	}
}

// List returns sorted live snapshots, optionally including finished jobs.
func (m *Manager) List(owner string, all bool) []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Snapshot{}
	for _, j := range m.jobs {
		if allowed(owner, j.view.Owner) && (all || j.view.Status == "running") {
			v := preview(j)
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Stop cancels a whole process group and waits for its result.
func (m *Manager) Stop(owner, id string) (Snapshot, error) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok || !allowed(owner, j.view.Owner) {
		m.mu.Unlock()
		return Snapshot{}, ErrNotFound
	}
	if j.view.Kind == "subagent" && j.view.Owner == owner {
		m.mu.Unlock()
		return Snapshot{}, errors.New("a child cannot stop its own task")
	}
	j.cancel()
	done := j.done
	m.mu.Unlock()
	<-done
	return m.View(owner, id)
}

// ReadOptions selects a stream, cursor and bounded source page. Grep filters lines
// within that source page; NextCursor still advances over the full source range.
type ReadOptions struct {
	Stream, Cursor      string
	Limit               int    // Bytes read before optional grep filtering; default is supplied by the tool.
	Grep                string // Go RE2 expression, empty disables filtering.
	Literal, IgnoreCase bool
}

// Read pages one named stream. EOF-relative cursors use eof:-N:bytes or eof:-N:lines.
func (m *Manager) Read(ctx context.Context, owner, id string, options ReadOptions) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok || !allowed(owner, j.view.Owner) {
		return nil, ErrNotFound
	}
	stream := options.Stream
	if stream == "" {
		stream = "stdout"
		if j.client != nil {
			stream = "stderr"
		}
	}
	if j.client != nil && stream != "stderr" {
		return nil, &lsp.Error{Code: "invalid_input", Message: "LSP stdout belongs to the protocol client; set stream=stderr for server diagnostics"}
	}
	buffer := j.stdout
	if stream == "stderr" {
		buffer = j.stderr
	} else if stream != "stdout" {
		return nil, errors.New("stream must be stdout or stderr")
	}
	page, err := buffer.Read(options.Cursor, options.Limit)
	if err != nil {
		return nil, err
	}
	var next any
	if page.NextCursor != fmt.Sprint(page.End) || j.view.Status == "running" {
		next = page.NextCursor
	}
	output := page.Output
	matches := []map[string]any{}
	if options.Grep != "" {
		pattern := options.Grep
		if options.Literal {
			pattern = regexp.QuoteMeta(pattern)
		}
		if options.IgnoreCase {
			pattern = "(?i)" + pattern
		}
		expression, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid output grep: %w", err)
		}
		var kept strings.Builder
		line, offset := page.Line, page.Start
		for _, text := range strings.SplitAfter(output, "\n") {
			if text == "" {
				continue
			}
			if expression.MatchString(strings.TrimSuffix(text, "\n")) {
				kept.WriteString(text)
				matches = append(matches, map[string]any{"line": line, "byte_offset": offset, "text": strings.TrimSuffix(text, "\n")})
			}
			line++
			offset += int64(len(text))
		}
		output = kept.String()
	}
	out := map[string]any{"job_id": id, "kind": j.view.Kind, "status": j.view.Status, "stream": stream, "output": output, "next_cursor": next, "truncated": page.Truncated, "start_byte": page.Start}
	if options.Grep != "" {
		out["matches"] = matches
	}
	if j.view.ExitCode != nil {
		out["exit_code"] = *j.view.ExitCode
	}
	return out, nil
}

func preview(j *job) Snapshot {
	v := j.view
	a, _ := j.stdout.Read("eof:0:bytes", 1)
	b, _ := j.stderr.Read("eof:0:bytes", 1)
	v.Stdout = j.stdout.Tail(10, 1024)
	v.Stderr = j.stderr.Tail(10, 1024)
	if v.Stdout != "" && v.Stderr != "" {
		v.Stdout = j.stdout.Tail(5, 512)
		v.Stderr = j.stderr.Tail(5, 512)
	}
	v.Truncated = a.Truncated || b.Truncated || int64(len(v.Stdout)) < a.End || int64(len(v.Stderr)) < b.End
	return v
}

// StartTask runs an owned background worker with the same capture pool and lifecycle
// as shell jobs. The worker must honor ctx and finish before its callback is joined.
func (m *Manager) StartTask(owner, kind, label string, background, wake bool, run func(context.Context, io.Writer, io.Writer) error) (string, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", errors.New("runtime ended")
	}
	ctx, cancel := context.WithCancel(m.ctx)
	startedAt := time.Now().UTC()
	j := &job{startedAt: startedAt, view: Snapshot{ID: history.NewID("job"), Kind: kind, Owner: owner, Label: label, Status: "running", StartedAt: startedAt.Format(time.RFC3339Nano)}, cancel: cancel, done: make(chan struct{}), background: background, stdout: m.pool.NewBuffer(capture.CallLimit / 2), stderr: m.pool.NewBuffer(capture.CallLimit / 2)}
	m.jobs[j.view.ID] = j
	m.wg.Add(1)
	launch := j.view
	m.mu.Unlock()
	if m.OnState != nil {
		m.OnState(launch)
	}
	go func() {
		defer m.wg.Done()
		err := run(ctx, j.stdout, j.stderr)
		m.mu.Lock()
		j.view.Status = "completed"
		if ctx.Err() != nil {
			j.view.Status = "cancelled"
		} else if err != nil {
			j.view.Status = "failed"
			fmt.Fprintln(j.stderr, err)
		}
		j.view.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		view := preview(j)
		view.WakeOnExit = wake
		m.mu.Unlock()
		if m.OnState != nil {
			m.OnState(view)
		}
		close(j.done)
		cancel()
		if background && m.Notify != nil {
			m.Notify(view)
		}
	}()
	return launch.ID, nil
}

// Close stops all jobs, waits for output capture, then discards every handle.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	var done []chan struct{}
	for _, j := range m.jobs {
		j.cancel()
		done = append(done, j.done)
	}
	m.mu.Unlock()
	for _, d := range done {
		<-d
	}
	m.wg.Wait()
	m.mu.Lock()
	m.jobs = map[string]*job{}
	m.pool.Clear()
	m.mu.Unlock()
}

// Live returns running job metadata without copying stdout/stderr capture tails.
// Unlike recorded history, these handles belong to this active manager only.
// Results are oldest-started first, with job ID breaking equal launch times.
func (m *Manager) Live() []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Snapshot
	for _, j := range m.jobs {
		if j.view.Status == "running" {
			out = append(out, j.view)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := m.jobs[out[i].ID].startedAt, m.jobs[out[j].ID].startedAt
		if a.Equal(b) {
			return out[i].ID < out[j].ID
		}
		return a.Before(b)
	})
	return out
}

// Metadata returns actor-visible jobs, including finished jobs, without reading
// output buffers. Results are copied and sorted by ID for deterministic context.
func (m *Manager) Metadata(owner string) []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Snapshot, 0, len(m.jobs))
	for _, j := range m.jobs {
		if allowed(owner, j.view.Owner) {
			v := j.view
			if v.ExitCode != nil {
				code := *v.ExitCode
				v.ExitCode = &code
			}
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
