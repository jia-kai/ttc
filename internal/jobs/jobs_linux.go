// Package jobs supervises bounded, session-local Linux shell process groups.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"ttc/internal/capture"
	"ttc/internal/prompts"

	"ttc/internal/history"
	"ttc/internal/lsp"
	"ttc/internal/managed"
)

// ErrNotFound identifies a stale or inaccessible job handle without exposing
// another actor's jobs. Callers can map it to their own error protocol.
var ErrNotFound = errors.New(prompts.JobNotFound)

// ExecutionTimeout is the default process deadline for ordinary shells.
const ExecutionTimeout = time.Hour

// Snapshot is an immutable job view. ExitCode is nil until an exit is observed.
type Snapshot struct {
	command               string             // Exact launch command; exposed only by explicit inspection.
	stopLabel             string             // Launch-time display identity, independent of capture output.
	Background            bool               `json:"background,omitempty"`
	Promoted              bool               `json:"promoted,omitempty"`
	TimeoutSeconds        *float64           `json:"timeout_seconds,omitempty"` // Nil means no execution deadline.
	ForegroundWaitSeconds *float64           `json:"max_fg_wait_seconds,omitempty"`
	CancellationReason    string             `json:"cancellation_reason,omitempty"`
	ID                    string             `json:"job_id"`
	Kind                  string             `json:"kind"`
	Owner                 string             `json:"owner_actor_id"`
	Label                 string             `json:"label"`
	RecordDir             string             `json:"record_dir,omitempty"`  // Watch-run records observed by a watch job.
	Grep                  *GrepOrigin        `json:"grep_origin,omitempty"` // Immutable filter and source snapshot bounds.
	Status                string             `json:"status"`
	Stdout                string             `json:"stdout"`
	Stderr                string             `json:"stderr"`
	StartedAt             string             `json:"started_at"` // UTC RFC3339Nano launch time.
	FinishedAt            string             `json:"finished_at,omitempty"`
	ExitCode              *int               `json:"exit_code,omitempty"`
	Signal                string             `json:"signal,omitempty"`
	Truncated             bool               `json:"truncated"`
	StdoutTruncated       bool               `json:"-"` // Per-stream preview state for boundaries that suppress stderr.
	StdoutSelection       *capture.Selection `json:"stdout_selection,omitempty"`
	StderrSelection       *capture.Selection `json:"stderr_selection,omitempty"`
}
type job struct {
	view                    Snapshot
	startedAt               time.Time // Immutable launch time; ordering never depends on capture updates.
	cmd                     *exec.Cmd
	ctx                     context.Context
	cancel                  context.CancelFunc
	done                    chan struct{}
	published               chan struct{} // Closed after all terminal callbacks return.
	promoted                chan struct{} // Closed once when foreground shell ownership moves to background.
	foregroundResultPending bool          // Reserved at launch; the original foreground call owns the first handoff.
	stdout, stderr          *capture.Buffer
	cancelReason            string // Protected by Manager.mu; first cancellation wins.
	client                  *lsp.Client
	output                  *capture.OutputOptions // Nil retains compact previews for non-shell tasks.
	inspector               Inspector
}

// Manager owns jobs for one runtime. Close cancels and joins all process groups.
type Manager struct {
	wg     sync.WaitGroup
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	jobs   map[string]*job
	scopes map[string]*managed.Scope
	closed bool
	Notify func(Snapshot)
	// OnState publishes launch and terminal states outside the manager lock.
	// Set before launching jobs; callbacks must not wait for this job to finish.
	OnState   func(Snapshot)
	pool      *capture.Pool
	grepScans chan struct{} // Bounds temporary whole-stream snapshots outside the capture pool.
}

// New constructs a live supervisor; foreground contexts do not own background jobs.
func New(ctx context.Context, notify func(Snapshot)) *Manager {
	ctx, cancel := context.WithCancel(ctx)
	return &Manager{ctx: ctx, cancel: cancel, jobs: map[string]*job{}, scopes: map[string]*managed.Scope{}, Notify: notify, pool: capture.NewPool(capture.SharedLimit), grepScans: make(chan struct{}, 2)}
}

// Start launches a closed-stdin shell in a dedicated process group.
// strict enables POSIX errexit and nounset; it does not enable pipefail.
// Zero timeout means no execution deadline; public defaults are applied by tools.
// A positive timeout is a process deadline independent of foreground waiting.
// output selects the model-facing payload independently of retained job_read data.
// label is whitespace-normalized and bounded to 80 code points;
// empty or whitespace selects "Shell command", never the command text.
// Foreground launches reserve their original result for WaitForeground before
// the process becomes visible to concurrent promotion or completion.
func (m *Manager) Start(owner, command, workdir string, timeout time.Duration, strict, background bool, output capture.OutputOptions, label string) (string, error) {
	return m.start(owner, command, workdir, timeout, strict, background, false, output, label)
}

// StartLSP launches a background stdio server. Protocol stdout is owned by the
// LSP client; only stderr is available through Read. Stop cancels the process group.
// output selects bounded stderr diagnostics; protocol stdout is never presented.
// label follows Start's rules, with "Language server" as fallback.
func (m *Manager) StartLSP(owner, command, workdir string, timeout time.Duration, strict bool, output capture.OutputOptions, label string) (string, error) {
	return m.start(owner, command, workdir, timeout, strict, true, true, output, label)
}

func (m *Manager) start(owner, command, workdir string, timeout time.Duration, strict, background, protocol bool, options capture.OutputOptions, label string) (string, error) {
	options, err := options.Normalize()
	if err != nil {
		return "", err
	}
	if timeout < 0 {
		return "", errors.New(prompts.JobTimeoutNonnegative)
	}
	label = compactLabel(label, "")
	stopLabel := label
	if stopLabel == "" {
		stopLabel = compactLabel(command, "")
	}
	if label == "" {
		label = prompts.JobLabelShell
		if protocol {
			label = prompts.JobLabelLSP
		}
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", errors.New(prompts.RuntimeEnded)
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
	j := &job{startedAt: startedAt, view: Snapshot{command: command, ID: history.NewID("job"), Kind: "shell", Owner: owner, Label: label, Status: "running", StartedAt: startedAt.Format(time.RFC3339Nano), Background: background}, cmd: cmd, cancel: cancel, done: make(chan struct{}), promoted: make(chan struct{}), stdout: m.pool.NewBuffer(capture.CallLimit / 2), stderr: m.pool.NewBuffer(capture.CallLimit / 2)}
	j.view.stopLabel = stopLabel
	if timeout > 0 {
		seconds := timeout.Seconds()
		j.view.TimeoutSeconds = &seconds
	}
	j.inspector = commandInspector
	j.foregroundResultPending = !background && !protocol
	j.ctx = ctx
	j.published = make(chan struct{})
	if !m.scopeLocked(owner).Register(m.cleanupCancel(j), j.published) {
		cancel()
		m.mu.Unlock()
		return "", errors.New(prompts.RuntimeEnded)
	}
	cmd.Stdout = j.stdout
	j.output = &options
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
			close(j.published)
			m.scopeLocked(owner).Release(j.published)
			m.mu.Unlock()
			return "", fmt.Errorf(prompts.JobLSPPipeFailure, err)
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
		close(j.published)
		m.scopeLocked(owner).Release(j.published)
		m.mu.Unlock()
		return "", e
	}
	if protocol {
		_ = writer.Close() // Only the child retains the stdout writer.
		j.client = lsp.New(ctx, workdir, input, output)
	}
	m.jobs[j.view.ID] = j
	m.wg.Add(1)
	launch := cloneSnapshot(j.view)
	m.mu.Unlock()
	if m.OnState != nil {
		m.OnState(launch)
	}
	go func() {
		defer m.wg.Done()
		defer func() { close(j.published); m.ReleaseOwned(owner, j.published) }()
		e := cmd.Wait() // Kill lingering descendants even if the group leader exits first.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if j.client != nil {
			j.client.Close()
		}
		m.mu.Lock()
		j.view.Status = "completed"
		if ctx.Err() != nil {
			j.view.Status = "cancelled"
			j.view.CancellationReason = cancellationReason(ctx, j)
		} else if e != nil {
			var exit *exec.ExitError
			if !errors.As(e, &exit) {
				j.view.Status = "failed"
				fmt.Fprintf(j.stderr, prompts.JobSupervisionFailure, e)
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
		j.view.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		view := preview(j)
		notify := j.view.Background
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
		return nil, &lsp.Error{Code: "not_found", Message: prompts.JobLSPNotFound}
	}
	if j.client == nil {
		m.mu.Unlock()
		return nil, &lsp.Error{Code: "invalid_input", Message: prompts.JobNotLSPServer}
	}
	if j.view.Status != "running" {
		m.mu.Unlock()
		return nil, &lsp.Error{Code: "job_not_running", Message: prompts.JobLSPExited}
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

// Wait observes exit; the first waiter on a live foreground shell also accepts
// its promotion handoff. Cancellation stops foreground work only.
func (m *Manager) Wait(ctx context.Context, owner, id string, update func(Snapshot)) (Snapshot, error) {
	return m.wait(ctx, owner, id, nil, update)
}

// WaitForeground waits up to budget before promoting the same shell process.
// The process deadline is never changed. A zero budget promotes immediately.
// The original call receives a nonterminal handoff after promotion, even when
// promotion and completion both precede waiter registration. Later observers
// receive the retained terminal state normally.
func (m *Manager) WaitForeground(ctx context.Context, owner, id string, budget time.Duration, update func(Snapshot)) (Snapshot, error) {
	return m.wait(ctx, owner, id, &budget, update)
}

func (m *Manager) wait(ctx context.Context, owner, id string, budget *time.Duration, update func(Snapshot)) (Snapshot, error) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok || !allowed(owner, j.view.Owner) {
		m.mu.Unlock()
		return Snapshot{}, ErrNotFound
	}
	done := j.done
	var expiry <-chan time.Time
	if budget != nil {
		seconds := budget.Seconds()
		j.view.ForegroundWaitSeconds = &seconds
		timer := time.NewTimer(*budget)
		defer timer.Stop()
		expiry = timer.C
	}
	var promoted <-chan struct{}
	if j.foregroundResultPending && (budget != nil || j.view.Status == "running") {
		promoted = j.promoted
		j.foregroundResultPending = false
	}
	m.mu.Unlock()
	// A claimed promotion owns the original tool result even if the process
	// finishes before this goroutine resumes. Terminal delivery then belongs
	// exclusively to the background completion callback.
	handoff := func() (Snapshot, bool) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if promoted != nil && j.view.Promoted {
			// Promotion already transferred terminal delivery to Notify. The
			// original call must return a handoff even when it registers after
			// completion; derive it without retaining a second output payload.
			view := preview(j)
			view.Status = "running"
			view.FinishedAt, view.Signal, view.CancellationReason = "", "", ""
			view.ExitCode = nil
			return view, true
		}
		return Snapshot{}, false
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if update != nil {
			m.mu.Lock()
			view := compactPreview(j)
			m.mu.Unlock()
			if view.Status == "running" {
				update(view)
			}
		}
		select {
		case <-done:
			if view, ok := handoff(); ok {
				return view, nil
			}
			return m.View(owner, id)
		case <-promoted:
			view, _ := handoff()
			return view, nil
		case <-ctx.Done():
			m.mu.Lock()
			if j.view.Background {
				view := preview(j)
				m.mu.Unlock()
				if promoted, ok := handoff(); ok {
					return promoted, nil
				}
				if view.Status != "running" {
					<-done
					return m.View(owner, id)
				}
				return view, nil
			}
			cancelJob(j, "caller_cancelled")
			m.mu.Unlock()
			<-done
			return m.View(owner, id)
		case <-expiry:
			// Caller cancellation still owns foreground work until promotion.
			if ctx.Err() != nil {
				expiry = nil
				continue
			}
			view, err := m.Promote(owner, id)
			if promoted, ok := handoff(); ok {
				return promoted, nil
			}
			if err == nil && view.Status != "running" {
				<-done
				return m.View(owner, id)
			}
			return view, err
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

// Stop cancels a whole process group and waits for its result. The returned
// snapshot contains metadata only; stopping never acquires captured output.
func (m *Manager) Stop(owner, id string) (Snapshot, error) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok || !allowed(owner, j.view.Owner) {
		m.mu.Unlock()
		return Snapshot{}, ErrNotFound
	}
	if j.view.Kind == "subagent" && j.view.Owner == owner {
		m.mu.Unlock()
		return Snapshot{}, errors.New(prompts.JobCannotStopOwnTask)
	}
	cancelJob(j, "explicit_stop")
	done := j.done
	m.mu.Unlock()
	<-done
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneSnapshot(j.view), nil
}

// ReadOptions selects a bounded stream page or creates a grep snapshot job.
// Grep searches the entire retained raw stream and requires an empty cursor.
type ReadOptions struct {
	Stream, Cursor      string
	Limit               int    // Source-byte page budget; default is supplied by the tool.
	Grep                string // Go RE2 expression, empty disables filtering.
	Literal, IgnoreCase bool
	StripANSI           *bool // Nil uses cleaned output; false selects raw capture.
}

// Read pages one named stream. EOF-relative cursors use eof:-N:bytes or eof:-N:lines.
func (m *Manager) Read(ctx context.Context, owner, id string, options ReadOptions) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.Grep != "" {
		return m.grep(ctx, owner, id, options)
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
		return nil, &lsp.Error{Code: "invalid_input", Message: prompts.JobLSPProtocolStdout}
	}
	buffer := j.stdout
	if stream == "stderr" {
		buffer = j.stderr
	} else if stream != "stdout" {
		return nil, errors.New(prompts.JobInvalidStream)
	}
	page, err := buffer.ReadView(options.Cursor, options.Limit, capture.StripEnabled(options.StripANSI))
	if err != nil {
		return nil, err
	}
	var next any
	if page.NextCursor != fmt.Sprint(page.End) || j.view.Status == "running" {
		next = page.NextCursor
	}
	out := map[string]any{"job_id": id, "kind": j.view.Kind, "status": j.view.Status, "stream": stream, "output": page.Output, "next_cursor": next, "truncated": page.Truncated, "start_byte": page.Start}
	if j.view.Grep != nil {
		origin := *j.view.Grep
		out["grep_origin"] = origin
		out["truncated"] = page.Truncated || origin.Truncated
	}
	if j.view.ExitCode != nil {
		out["exit_code"] = *j.view.ExitCode
	}
	return out, nil
}

func preview(j *job) Snapshot {
	if j.output == nil {
		return compactPreview(j)
	}
	v := cloneSnapshot(j.view)
	a, b := capture.SelectOutput(j.stdout, j.stderr, *j.output)
	v.Stdout, v.Stderr = a.Output, b.Output
	v.StdoutSelection, v.StderrSelection = &a, &b
	v.StdoutTruncated = a.OmittedBytes > 0 || a.CleaningError != ""
	v.Truncated = v.StdoutTruncated || b.OmittedBytes > 0 || b.CleaningError != ""
	return v
}

func compactPreview(j *job) Snapshot {
	v := cloneSnapshot(j.view)
	strip := j.output == nil || capture.StripEnabled(j.output.StripANSI)
	tail := func(buffer *capture.Buffer, lines, limit int, selection **capture.Selection) string {
		text, err := buffer.TailView(lines, limit, strip)
		if err != nil {
			*selection = &capture.Selection{CleaningError: err.Error()}
			v.Truncated = true
		}
		return text
	}
	a, _ := j.stdout.Read("eof:0:bytes", 1)
	b, _ := j.stderr.Read("eof:0:bytes", 1)
	v.Stdout = tail(j.stdout, 10, 1024, &v.StdoutSelection)
	v.Stderr = tail(j.stderr, 10, 1024, &v.StderrSelection)
	if v.Stdout != "" && v.Stderr != "" {
		v.Stdout = tail(j.stdout, 5, 512, &v.StdoutSelection)
		v.Stderr = tail(j.stderr, 5, 512, &v.StderrSelection)
	}
	v.StdoutTruncated = a.Truncated || int64(len(v.Stdout)) < a.End || v.StdoutSelection != nil && v.StdoutSelection.CleaningError != ""
	v.Truncated = v.Truncated || v.StdoutTruncated || b.Truncated || int64(len(v.Stderr)) < b.End
	return v
}

// TaskResult carries optional observed workload exit metadata. A nil ExitCode
// means no numeric exit was observed; Signal is empty unless signaled termination
// was observed. A nonzero exit does not itself indicate worker supervision failure.
type TaskResult struct {
	ExitCode *int
	Signal   string
}

// StartTask runs an owned background worker with the same capture pool and lifecycle
// as shell jobs. The worker must honor ctx and finish before its callback is joined.
// Returned exit metadata is copied and published atomically with terminal status
// and captured output. Worker errors report supervision failure, not exit status.
func (m *Manager) StartTask(owner string, options TaskOptions, run func(context.Context, io.Writer, io.Writer) (TaskResult, error)) (string, error) {
	label := compactLabel(options.Label, "")
	stopLabel := label
	if stopLabel == "" {
		stopLabel = compactLabel(options.command, "")
	}
	if label == "" {
		label = compactLabel(options.defaultLabel, options.Kind)
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", errors.New(prompts.RuntimeEnded)
	}
	ctx, cancel := context.WithCancel(m.ctx)
	startedAt := time.Now().UTC()
	metadata := Snapshot{command: options.command, ID: history.NewID("job"), Kind: options.Kind, Label: label, RecordDir: options.recordDir, Owner: owner, Status: "running", StartedAt: startedAt.Format(time.RFC3339Nano), Background: options.Background}
	metadata.stopLabel = stopLabel
	inspector := options.Inspector
	if inspector == nil {
		inspector = taskInspector
	}
	var output *capture.OutputOptions
	if options.Output != nil {
		normalized, err := options.Output.Normalize()
		if err != nil {
			cancel()
			m.mu.Unlock()
			return "", err
		}
		output = &normalized
	}
	j := &job{ctx: ctx, startedAt: startedAt, view: metadata, inspector: inspector, output: output, cancel: cancel, done: make(chan struct{}), stdout: m.pool.NewBuffer(capture.CallLimit / 2), stderr: m.pool.NewBuffer(capture.CallLimit / 2)}
	j.published = make(chan struct{})
	// An assignment supervisor closes its resource scope from its deferred
	// completion path, so it must live outside that scope.
	if options.Kind != "subagent" && !m.scopeLocked(owner).Register(m.cleanupCancel(j), j.published) {
		cancel()
		m.mu.Unlock()
		return "", errors.New(prompts.RuntimeEnded)
	}
	m.jobs[j.view.ID] = j
	m.wg.Add(1)
	launch := cloneSnapshot(j.view)
	m.mu.Unlock()
	if m.OnState != nil {
		m.OnState(launch)
	}
	go func() {
		defer m.wg.Done()
		defer func() { close(j.published); m.ReleaseOwned(owner, j.published) }()
		result, err := run(ctx, j.stdout, j.stderr)
		m.mu.Lock()
		if result.ExitCode != nil {
			code := *result.ExitCode
			j.view.ExitCode = &code
		}
		j.view.Signal = result.Signal
		j.view.Status = "completed"
		if ctx.Err() != nil {
			j.view.Status = "cancelled"
			j.view.CancellationReason = cancellationReason(ctx, j)
		} else if err != nil {
			j.view.Status = "failed"
			fmt.Fprintln(j.stderr, err)
		}
		j.view.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		view := preview(j)
		m.mu.Unlock()
		if m.OnState != nil {
			m.OnState(view)
		}
		close(j.done)
		cancel()
		if view.Background && m.Notify != nil {
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
	var scopes []*managed.Scope
	for _, scope := range m.scopes {
		scopes = append(scopes, scope)
	}
	for _, j := range m.jobs {
		cancelJob(j, "runtime_cleanup")
		done = append(done, j.done)
	}
	m.mu.Unlock()
	for _, scope := range scopes {
		scope.Cancel()
	}
	for _, scope := range scopes {
		scope.Join()
	}
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
			out = append(out, cloneSnapshot(j.view))
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
			v := cloneSnapshot(j.view)
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
