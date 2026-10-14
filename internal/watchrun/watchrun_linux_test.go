package watchrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestRunAndObserve(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	var out, stderr bytes.Buffer
	code, err := Run(dir, []string{"/bin/sh", "-c", "cat; printf error >&2; exit 7"}, strings.NewReader("input\x00bytes\n"), &out, &stderr)
	if err != nil || code != 7 {
		t.Fatalf("run: %d %v", code, err)
	}
	if out.String() != "input\x00bytes\n" || stderr.String() != "error" {
		t.Fatalf("tee %q %q", out.String(), stderr.String())
	}
	id, err := ReadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	var a, b bytes.Buffer
	c, err := Observe(context.Background(), dir, id, &a, &b)
	if err != nil || c.ExitCode != 7 || a.String() != out.String() || b.String() != stderr.String() {
		t.Fatalf("observe: %+v %v %q %q", c, err, a.String(), b.String())
	}
	if _, err := Run(dir, []string{"true"}, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("reused directory")
	}
}

func TestCompletionAfterDescendantDrain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "record")
	var out bytes.Buffer
	code, err := Run(dir, []string{"/bin/sh", "-c", `(sleep 0.05; printf late) & printf early`}, nil, &out, io.Discard)
	if err != nil || code != 0 || out.String() != "earlylate" {
		t.Fatalf("drain: %d %v %q", code, err, out.String())
	}
	id, err := ReadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	var captured bytes.Buffer
	_, err = Observe(context.Background(), dir, id, &captured, io.Discard)
	if err != nil || captured.String() != "earlylate" {
		t.Fatalf("record: %q %v", captured.String(), err)
	}
}

func TestWrapperHelper(t *testing.T) {
	if os.Getenv("TTC_WATCH_HELPER") == "" {
		return
	}
	code, err := Run(os.Getenv("TTC_WATCH_DIR"), []string{"/bin/sh", "-c", `echo $$ > "$TTC_CHILD_PID"; exec sleep 60`}, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		os.Exit(99)
	}
	os.Exit(code)
}
func wrapper(t *testing.T) (*exec.Cmd, string, Identity, int) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "record")
	pidFile := filepath.Join(root, "pid")
	cmd := exec.Command(os.Args[0], "-test.run=^TestWrapperHelper$")
	cmd.Env = append(os.Environ(), "TTC_WATCH_HELPER=1", "TTC_WATCH_DIR="+dir, "TTC_CHILD_PID="+pidFile)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		id, err := ReadIdentity(dir)
		pid, pe := os.ReadFile(pidFile)
		if err == nil && pe == nil {
			n, _ := strconv.Atoi(strings.TrimSpace(string(pid)))
			if n > 0 {
				return cmd, dir, id, n
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("wrapper did not start")
	return nil, "", Identity{}, 0
}
func TestForcedWrapperDeath(t *testing.T) {
	cmd, dir, id, pid := wrapper(t)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := Observe(ctx, dir, id, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("expected unknown: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, state, err := processIdentity(pid)
		if os.IsNotExist(err) || state == "Z" || state == "X" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatal("direct child survived wrapper death")
}
func TestForwardSignal(t *testing.T) {
	cmd, dir, id, _ := wrapper(t)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	if err == nil || cmd.ProcessState.ExitCode() != 143 {
		t.Fatalf("exit: %v %v", cmd.ProcessState, err)
	}
	c, err := Observe(context.Background(), dir, id, io.Discard, io.Discard)
	if err != nil || c.ExitCode != 143 || c.Signal == "" {
		t.Fatalf("completion %+v %v", c, err)
	}
}
func TestStaleIdentityAndMissingLogs(t *testing.T) {
	dir := t.TempDir()
	start, _, err := processIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{Version: 1, ID: "record", PID: os.Getpid(), Start: start + "0", Boot: strings.TrimSpace(string(boot)), Command: []string{"true"}}
	if err := atomicJSON(dir, "identity.json", id); err != nil {
		t.Fatal(err)
	}
	_, err = Observe(context.Background(), dir, id, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("stale: %v", err)
	}
	if err := atomicJSON(dir, "completion.json", Completion{ID: id.ID, FinishedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	_, err = Observe(context.Background(), dir, id, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "log") {
		t.Fatalf("missing log: %v", err)
	}
}

func completedFixture(t *testing.T) (string, Identity) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "record")
	if _, err := Run(dir, []string{"/bin/true"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	id, err := ReadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, id
}

func TestNonregularRecordsAndLogs(t *testing.T) {
	for _, name := range []string{"identity.json", "completion.json", "stdout", "stderr"} {
		for _, kind := range []string{"fifo", "directory", "symlink"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				dir, id := completedFixture(t)
				path := filepath.Join(dir, name)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				var err error
				switch kind {
				case "fifo":
					err = syscall.Mkfifo(path, 0600)
				case "directory":
					err = os.Mkdir(path, 0700)
				case "symlink":
					err = os.Symlink("/dev/zero", path)
				}
				if err != nil {
					t.Fatal(err)
				}
				started := time.Now()
				if name == "identity.json" {
					_, err = ReadIdentity(dir)
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					_, err = Observe(ctx, dir, id, io.Discard, io.Discard)
				}
				if err == nil || errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
					t.Fatalf("special file was not promptly rejected: %v", err)
				}
			})
		}
	}
}

func TestCompletionRequiresKnownExitCode(t *testing.T) {
	dir, id := completedFixture(t)
	data, err := json.Marshal(map[string]any{"id": id.ID, "finished_at": time.Now().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "completion.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = Observe(context.Background(), dir, id, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("inferred exit success: %v", err)
	}
}

func TestCompletionTimestampAndExitDomain(t *testing.T) {
	for _, test := range []struct {
		name, timestamp string
		code            any
		valid           bool
	}{
		{"invalid_timestamp", "not-a-time", 0, false},
		{"invalid_date", "2026-02-30T00:00:00Z", 0, false},
		{"null_exit", "2026-10-13T00:00:00Z", nil, false},
		{"negative_exit", "2026-10-13T00:00:00Z", -1, false},
		{"large_exit", "2026-10-13T00:00:00Z", 256, false},
		{"fractional_exit", "2026-10-13T00:00:00Z", 0.5, false},
		{"zero_exit", "2026-10-13T00:00:00.123Z", 0, true},
		{"maximum_exit", "2026-10-13T00:00:00+01:00", 255, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir, id := completedFixture(t)
			data, err := json.Marshal(map[string]any{"id": id.ID, "finished_at": test.timestamp, "exit_code": test.code})
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, "completion.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			completion, err := Observe(context.Background(), dir, id, io.Discard, io.Discard)
			if test.valid {
				if err != nil || completion.ID != id.ID {
					t.Fatalf("valid completion: %+v %v", completion, err)
				}
			} else if err == nil || completion.ID != "" {
				t.Fatalf("invalid completion accepted as known outcome: %+v %v", completion, err)
			}
		})
	}
}

func TestMetadataLimit(t *testing.T) {
	for _, name := range []string{"identity.json", "completion.json"} {
		t.Run(name, func(t *testing.T) {
			dir, id := completedFixture(t)
			if err := os.WriteFile(filepath.Join(dir, name), bytes.Repeat([]byte("x"), metadataLimit+1), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			if name == "identity.json" {
				_, err = ReadIdentity(dir)
			} else {
				_, err = Observe(context.Background(), dir, id, io.Discard, io.Discard)
			}
			if err == nil || !strings.Contains(err.Error(), "64 KiB") {
				t.Fatalf("metadata: %v", err)
			}
		})
	}
}

type countWriter struct{ bytes int }

func (w *countWriter) Write(p []byte) (int, error) { w.bytes += len(p); return len(p), nil }

type cancelWriter struct {
	cancel context.CancelFunc
	bytes  int
}

func (w *cancelWriter) Write(p []byte) (int, error) {
	w.bytes += len(p)
	w.cancel()
	return len(p), nil
}

func TestBoundedDrainPassAndCancellation(t *testing.T) {
	dir, id := completedFixture(t)
	if err := os.Truncate(filepath.Join(dir, "stdout"), 64<<20); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stderr"), []byte("cancel"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := new(countWriter)
	stderr := &cancelWriter{cancel: cancel}
	_, err := Observe(ctx, dir, id, out, stderr)
	if !errors.Is(err, context.Canceled) || out.bytes != drainPassLimit || stderr.bytes != len("cancel") {
		t.Fatalf("pass stdout=%d stderr=%d err=%v", out.bytes, stderr.bytes, err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestObservationDetectsLogReplacement(t *testing.T) {
	dir, id := completedFixture(t)
	path := filepath.Join(dir, "stdout")
	if err := os.Truncate(path, 1<<20); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, "replacement")
	f, err := os.Create(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 20); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	replaced := false
	out := writerFunc(func(p []byte) (int, error) {
		if !replaced {
			replaced = true
			if err := os.Rename(replacement, path); err != nil {
				return 0, err
			}
		}
		return len(p), nil
	})
	_, err = Observe(context.Background(), dir, id, out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("log replacement undetected: %v", err)
	}
}

func TestHotProducerObservationCancellation(t *testing.T) {
	dir, id := completedFixture(t)
	if err := os.Remove(filepath.Join(dir, "completion.json")); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "stdout"), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		data := make([]byte, 32768)
		for ctx.Err() == nil {
			if _, err := f.Write(data); err != nil {
				return
			}
		}
	}()
	started := time.Now()
	_, err = Observe(ctx, dir, id, io.Discard, io.Discard)
	cancel()
	<-producerDone
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("hot producer stop: %v %v", time.Since(started), err)
	}
}

func TestLogFailureStillDrainsTerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	var terminal bytes.Buffer
	writer := &tee{log: f, terminal: &terminal}
	for i := 0; i < 3; i++ {
		if n, err := writer.Write([]byte("data")); err != nil || n != 4 {
			t.Fatalf("drain %d %v", n, err)
		}
	}
	if !errors.Is(writer.err, os.ErrClosed) || terminal.String() != "datadatadata" {
		t.Fatalf("log failure hidden: %v %q", writer.err, terminal.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) { return 0, syscall.ENOSPC }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }
func TestShortWriteRecorded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "record")
	code, err := Run(dir, []string{"/bin/sh", "-c", "printf data; exit 8"}, nil, shortWriter{}, io.Discard)
	if code != 8 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write: %d %v", code, err)
	}
	id, err := ReadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Observe(context.Background(), dir, id, io.Discard, io.Discard)
	if err == nil || !strings.Contains(c.Error, "short write") || c.ExitCode != 8 {
		t.Fatalf("record: %+v %v", c, err)
	}
}

type blockingSink struct {
	entered, release chan struct{}
	once             sync.Once
}

func (w *blockingSink) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

func TestDrainDeadlineDoesNotPreemptBlockedSink(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "record")
	writer := &blockingSink{entered: make(chan struct{}), release: make(chan struct{})}
	type result struct {
		code int
		err  error
	}
	finished := make(chan result, 1)
	go func() {
		code, err := Run(dir, []string{"/bin/sh", "-c", "printf data; exit 7"}, nil, writer, io.Discard)
		finished <- result{code, err}
	}()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(writer.release) }) }
	joined := false
	defer func() {
		release()
		if !joined {
			<-finished
		}
	}()
	select {
	case <-writer.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("sink was not entered")
	}
	// The four-byte printf fits in the pipe and the direct command exits.
	// The capture worker is instead blocked in a caller-supplied Write.
	select {
	case got := <-finished:
		joined = true
		t.Fatalf("blocked sink was unexpectedly preempted: %+v", got)
	case <-time.After(DrainTimeout + 500*time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(dir, "completion.json")); !os.IsNotExist(err) {
		t.Fatalf("published completion before sink joined: %v", err)
	}
	release()
	got := <-finished
	joined = true
	if got.code != 7 || got.err == nil || !strings.Contains(got.err.Error(), "incomplete") {
		t.Fatalf("released sink result: %+v", got)
	}
}

func TestDescendantPipeDrainBound(t *testing.T) {
	for _, code := range []int{0, 7} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			dir := filepath.Join(root, "record")
			pidFile := filepath.Join(root, "pid")
			cleanup := func() {
				data, _ := os.ReadFile(pidFile)
				pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
				if pid > 0 {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
			defer cleanup()
			type result struct {
				code int
				err  error
			}
			finished := make(chan result, 1)
			started := time.Now()
			go func() {
				c, e := Run(dir, []string{"/bin/sh", "-c", `sleep 60 & echo $! > "$1"; printf early; exit "$2"`, "--", pidFile, strconv.Itoa(code)}, nil, io.Discard, io.Discard)
				finished <- result{c, e}
			}()
			var got result
			select {
			case got = <-finished:
			case <-time.After(DrainTimeout + 3*time.Second):
				cleanup()
				<-finished
				t.Fatal("drain did not terminate within bound")
			}
			if got.code != code || got.err == nil || !strings.Contains(got.err.Error(), "incomplete") {
				t.Fatalf("run: %+v", got)
			}
			if elapsed := time.Since(started); elapsed < DrainTimeout || elapsed > DrainTimeout+3*time.Second {
				t.Fatalf("drain time: %v", elapsed)
			}
			id, err := ReadIdentity(dir)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			c, err := Observe(context.Background(), dir, id, &out, io.Discard)
			if err == nil || c.ExitCode != code || !strings.Contains(c.Error, "incomplete") || out.String() != "early" {
				t.Fatalf("completion: %+v %v output=%q", c, err, out.String())
			}
		})
	}
}

func TestSinkFailureRecorded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	_, err := Run(dir, []string{"/bin/sh", "-c", "printf data"}, nil, failingWriter{}, io.Discard)
	if err == nil {
		t.Fatal("sink failure hidden")
	}
	data, err := os.ReadFile(filepath.Join(dir, "completion.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c Completion
	if err = json.Unmarshal(data, &c); err != nil || c.Error == "" {
		t.Fatalf("completion: %+v %v", c, err)
	}
}
