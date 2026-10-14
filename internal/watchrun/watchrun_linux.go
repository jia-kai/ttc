// Package watchrun records externally owned, noninteractive Linux commands.
// Observers only read records: they never signal or adopt the workload.
package watchrun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"ttc/internal/prompts"
)

// DrainTimeout sets the pipe-reader deadline after the direct command exits.
// At expiry the wrapper closes its read ends and marks output incomplete, but
// joins copying workers; blocked sink writes can delay finalization beyond this
// duration. The known command status is preserved; descendants are not adopted
// or killed by this deadline.
const DrainTimeout = 5 * time.Second

const metadataLimit = 64 << 10
const drainPassLimit = 256 << 10 // Bytes per stream before returning to observation checks.

// Identity is written once before launching a workload. Linux boot and process
// start identities prevent PID reuse from being mistaken for a live wrapper.
type Identity struct {
	Version   int      `json:"version"`
	ID        string   `json:"id"`
	PID       int      `json:"pid"`
	Start     string   `json:"start_ticks"`
	Boot      string   `json:"boot_id"`
	Command   []string `json:"command"`
	StartedAt string   `json:"started_at"`
}

// Completion is published atomically after command wait and output copying finish.
// Pipe readers have a deadline, but blocked sink writes can delay publication.
// Error denotes wrapper/logging failure or incomplete drain, not a nonzero
// command exit. ExitCode preserves the known direct command status even on error.
type Completion struct {
	ID         string `json:"id"`
	ExitCode   int    `json:"exit_code"`
	Signal     string `json:"signal,omitempty"`
	Error      string `json:"error,omitempty"`
	FinishedAt string `json:"finished_at"`
}

func atomicJSON(dir, name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(data)+1 > metadataLimit {
		return errors.New(prompts.WatchMetadataTooLarge)
	}
	f, err := os.CreateTemp(dir, ".record-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, name))
}

func processIdentity(pid int) (start, state string, err error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", "", err
	}
	// comm is parenthesized and may itself contain spaces and parentheses.
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return "", "", errors.New(prompts.WatchInvalidProcessStat)
	}
	fields := strings.Fields(string(b[end+1:]))
	if len(fields) < 20 {
		return "", "", errors.New(prompts.WatchShortProcessStat)
	}
	return fields[19], fields[0], nil
}

// Run creates recordDir exclusively, inherits stdin, and tees the exact command's
// output. It returns the command's shell-style exit status and wrapper failures.
// Launch this wrapper independently (for example in tmux) to outlive TTC.
func Run(recordDir string, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(argv) == 0 {
		return 1, errors.New("watch-run requires a command after --.")
	}
	if err := os.Mkdir(recordDir, 0700); err != nil {
		return 1, fmt.Errorf("create unique record directory: %w", err)
	}
	start, _, err := processIdentity(os.Getpid())
	if err != nil {
		return 1, err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return 1, err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return 1, fmt.Errorf("generate watch-run identity: %w", err)
	}
	id := Identity{Version: 1, ID: hex.EncodeToString(token[:]), PID: os.Getpid(), Start: start, Boot: strings.TrimSpace(string(boot)), Command: argv, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = atomicJSON(recordDir, "identity.json", id); err != nil {
		return 1, err
	}
	c := Completion{ID: id.ID, ExitCode: 1}
	code, runErr := execute(recordDir, argv, stdin, stdout, stderr, &c)
	c.ExitCode = code
	c.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if runErr != nil {
		c.Error = runErr.Error()
	}
	if err = atomicJSON(recordDir, "completion.json", c); err != nil {
		return 1, errors.Join(runErr, fmt.Errorf("publish watch-run completion: %w", err))
	}
	return code, runErr
}

// tee deliberately keeps draining even when a sink fails, so logging failures
// cannot leave the child blocked on a full pipe. execute joins all writer calls.
type tee struct {
	log      *os.File
	terminal io.Writer
	err      error
}

func (t *tee) Write(p []byte) (int, error) {
	_, a := t.log.Write(p)
	n, b := t.terminal.Write(p)
	if b == nil && n != len(p) {
		b = io.ErrShortWrite
	}
	// Retain the first failure only: a persistently failed sink must not grow
	// an unbounded error chain while we continue draining command output.
	if t.err == nil {
		t.err = errors.Join(a, b)
	}
	return len(p), nil
}
func execute(dir string, argv []string, stdin io.Reader, stdout, stderr io.Writer, c *Completion) (int, error) {
	out, err := os.OpenFile(filepath.Join(dir, "stdout"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 1, err
	}
	defer out.Close()
	eout, err := os.OpenFile(filepath.Join(dir, "stderr"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 1, err
	}
	defer eout.Close()
	a, b := &tee{log: out, terminal: stdout}, &tee{log: eout, terminal: stderr}
	readOut, writeOut, err := os.Pipe()
	if err != nil {
		return 1, err
	}
	defer readOut.Close()
	defer writeOut.Close()
	readErr, writeErr, err := os.Pipe()
	if err != nil {
		return 1, err
	}
	defer readErr.Close()
	defer writeErr.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = stdin
	cmd.Stdout = writeOut
	cmd.Stderr = writeErr
	cmd.WaitDelay = DrainTimeout
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	// Linux Pdeathsig tracks the creating OS thread, not merely the Go process.
	// Keep that thread alive through Wait (including drain). Go's Linux fork/exec
	// also checks the parent PID after installing Pdeathsig to close the fork race.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)
	if err = cmd.Start(); err != nil {
		return 1, err
	}
	_ = writeOut.Close()
	_ = writeErr.Close()
	drained := make(chan error, 2)
	go func() { _, e := io.Copy(a, readOut); drained <- e }()
	go func() { _, e := io.Copy(b, readErr); drained <- e }()
	done := make(chan struct{})
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		for {
			select {
			case s := <-signals:
				_ = syscall.Kill(-cmd.Process.Pid, s.(syscall.Signal))
			case <-done:
				return
			}
		}
	}()
	err = cmd.Wait()
	close(done)
	<-joined
	// Own these pipes explicitly rather than relying solely on Cmd.WaitDelay:
	// os/exec suppresses its drain error when the command exits nonzero.
	// Stop signal forwarding at direct-command wait, rather than leaving it
	// active throughout delayed drain where numeric group IDs can be reused.
	timer := time.NewTimer(DrainTimeout)
	defer timer.Stop()
	var drainErr error
	for remaining := 2; remaining > 0; remaining-- {
		select {
		case e := <-drained:
			drainErr = errors.Join(drainErr, e)
		case <-timer.C:
			drainErr = errors.Join(drainErr, errors.New(prompts.WatchDrainIncomplete))
			_ = readOut.Close()
			_ = readErr.Close()
			// Closing pollable pipes unblocks both readers; join before reading
			// tee errors or releasing the locked spawning thread.
			for ; remaining > 0; remaining-- {
				<-drained
			}
		}
	}
	code := cmd.ProcessState.ExitCode()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		c.Signal = ws.Signal().String()
		code = 128 + int(ws.Signal())
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		err = nil
	}
	return code, errors.Join(err, drainErr, a.err, b.err, out.Sync(), eout.Sync(), out.Close(), eout.Close())
}

// openRegular uses nonblocking open before inspecting the descriptor, so a FIFO
// or a path replaced between lookup and open cannot strand an observer. Symlinks
// are rejected as well; wrapper-created records are ordinary files.
func openRegular(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New(prompts.WatchNonregularFile)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func readMetadata(path string) ([]byte, error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, metadataLimit+1))
	if err == nil && len(data) > metadataLimit {
		err = errors.New(prompts.WatchMetadataTooLarge)
	}
	return data, err
}

// ReadIdentity validates the immutable record without requiring a live wrapper.
func ReadIdentity(dir string) (Identity, error) {
	var id Identity
	b, err := readMetadata(filepath.Join(dir, "identity.json"))
	if err != nil {
		return id, err
	}
	if err = json.Unmarshal(b, &id); err != nil {
		return id, err
	}
	if id.Version != 1 || id.ID == "" || id.PID <= 0 || id.Start == "" || id.Boot == "" || len(id.Command) == 0 {
		return id, errors.New(prompts.WatchInvalidIdentity)
	}
	return id, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// Observe copies growing regular-file logs in bounded, cancellable passes until
// completion or cancellation. Metadata is capped at 64 KiB; special files and
// symlinks are rejected. Writers must return promptly (jobs uses memory capture).
// Missing, stale, or zombie wrappers without completion have unknown outcomes,
// never success.
func Observe(ctx context.Context, dir string, id Identity, stdout, stderr io.Writer) (Completion, error) {
	files := []string{"stdout", "stderr"}
	writers := []io.Writer{stdout, stderr}
	offsets := []int64{0, 0}
	logFiles := make([]os.FileInfo, len(files))
	copyBuffer := make([]byte, 32<<10)
	drain := func(final bool) (bool, error) {
		caughtUp := true
		for i, name := range files {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			f, err := openRegular(filepath.Join(dir, name))
			if errors.Is(err, os.ErrNotExist) && !final {
				continue
			}
			if err != nil {
				return false, fmt.Errorf(prompts.WatchReadLog, name, err)
			}
			stat, err := f.Stat()
			if err == nil && logFiles[i] != nil && !os.SameFile(logFiles[i], stat) {
				err = errors.New(prompts.WatchLogReplaced)
			}
			if err == nil {
				logFiles[i] = stat
			}
			if err == nil && stat.Size() < offsets[i] {
				err = errors.New(prompts.WatchLogTruncated)
			}
			if err == nil {
				_, err = f.Seek(offsets[i], io.SeekStart)
			}
			if err == nil {
				var n int64
				remaining := stat.Size() - offsets[i]
				if remaining > drainPassLimit {
					remaining = drainPassLimit
					caughtUp = false
				}
				if remaining > 0 {
					n, err = io.CopyBuffer(writers[i], contextReader{ctx: ctx, reader: io.LimitReader(f, remaining)}, copyBuffer)
					offsets[i] += n
				}
			}
			closeErr := f.Close()
			if err == nil {
				err = closeErr
			}
			if err != nil {
				return false, err
			}
		}
		return caughtUp, nil
	}
	completion := func() (Completion, bool, error) {
		var c Completion
		b, err := readMetadata(filepath.Join(dir, "completion.json"))
		if errors.Is(err, os.ErrNotExist) {
			return c, false, nil
		}
		if err != nil {
			return c, false, err
		}
		var record struct {
			Completion
			ExitCode *int `json:"exit_code"`
		}
		err = json.Unmarshal(b, &record)
		c = record.Completion
		if err == nil && (c.ID != id.ID || c.FinishedAt == "" || record.ExitCode == nil || *record.ExitCode < 0 || *record.ExitCode > 255) {
			err = errors.New(prompts.WatchInvalidCompletion)
		}
		if err == nil {
			c.ExitCode = *record.ExitCode
			if _, timestampErr := time.Parse(time.RFC3339Nano, c.FinishedAt); timestampErr != nil {
				err = errors.New(prompts.WatchInvalidCompletion)
			}
		}
		if err != nil {
			return Completion{}, true, err
		}
		return c, true, err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return Completion{}, err
	}
	bootID := strings.TrimSpace(string(boot))
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return Completion{}, err
		}
		c, finished, err := completion()
		if err != nil {
			return c, err
		}
		caughtUp, err := drain(finished)
		if err != nil {
			return c, err
		}
		if finished {
			if !caughtUp {
				continue
			}
			if c.Error != "" {
				return c, errors.New(c.Error)
			}
			return c, nil
		}
		start, state, e := processIdentity(id.PID)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return c, e
		}
		if e != nil || start != id.Start || bootID != id.Boot || state == "Z" || state == "X" {
			// Publication can race the first absence check; recheck before declaring unknown.
			c, finished, err = completion()
			if err != nil {
				return c, err
			}
			if finished {
				continue // Next pass observes completion and boundedly drains its logs.
			}
			return c, errors.New(prompts.WatchOutcomeUnknown)
		}
		if !caughtUp {
			continue
		}
		select {
		case <-ctx.Done():
			return c, ctx.Err()
		case <-ticker.C:
		}
	}
}
