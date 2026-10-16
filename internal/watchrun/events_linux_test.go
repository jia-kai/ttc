package watchrun

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func liveFixture(t *testing.T) (string, Identity) {
	t.Helper()
	dir := t.TempDir()
	start, _, err := processIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{Version: 1, ID: "live", PID: os.Getpid(), Start: start, Boot: strings.TrimSpace(string(boot)), Command: []string{"test"}}
	for _, name := range []string{"stdout", "stderr"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, id
}

func requireInotify(t *testing.T, dir string, id Identity) {
	t.Helper()
	w := newEventWaiter(context.Background(), dir, id, id.Boot, observationBackup, true)
	defer w.close()
	if w.inotify < 0 {
		t.Skip("inotify unavailable")
	}
}

func publishCompletion(t *testing.T, dir string, id Identity) {
	t.Helper()
	if err := atomicJSON(dir, "completion.json", Completion{ID: id.ID, ExitCode: 7, FinishedAt: time.Now().Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("event did not wake observer before polling backup")
		var zero T
		return zero
	}
}

func TestObserveLiveEventsMultipleWatchers(t *testing.T) {
	dir, id := liveFixture(t)
	requireInotify(t, dir, id)
	if err := os.WriteFile(filepath.Join(dir, "stdout"), []byte("initial"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const observers = 3
	writes := make([]chan string, observers)
	results := make([]chan error, observers)
	var cancelFirst context.CancelFunc
	for i := range observers {
		writes[i], results[i] = make(chan string, 8), make(chan error, 1)
		observerCtx, observerCancel := context.WithCancel(ctx)
		defer observerCancel()
		if i == 0 {
			// Keep one observer independently cancellable while its peers run.
			cancelFirst = observerCancel
		}
		go func() {
			c, err := Observe(observerCtx, dir, id, writerFunc(func(p []byte) (int, error) {
				writes[i] <- string(p)
				return len(p), nil
			}), io.Discard)
			if err == nil && c.ExitCode != 7 {
				err = errors.New("wrong completion")
			}
			results[i] <- err
		}()
	}
	for i := range observers {
		if got := receive(t, writes[i]); got != "initial" {
			t.Fatalf("initial output %q", got)
		}
	}
	cancelFirst()
	if err := receive(t, results[0]); !errors.Is(err, context.Canceled) {
		t.Fatalf("independent cancellation: %v", err)
	}
	// The writers report the initial scan, after watches have been installed.
	// No sleep is needed: writes racing entry into wait must remain queued.
	f, err := os.OpenFile(filepath.Join(dir, "stdout"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("live"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()
	for i := 1; i < observers; i++ {
		if got := receive(t, writes[i]); got != "live" {
			t.Fatalf("live output %q", got)
		}
	}
	publishCompletion(t, dir, id)
	for i := 1; i < observers; i++ {
		if err := receive(t, results[i]); err != nil {
			t.Fatal(err)
		}
	}
}

func TestObserveLivePidfdDeath(t *testing.T) {
	cmd, dir, id, _ := wrapper(t)
	fd, err := unix.PidfdOpen(id.PID, 0)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Skipf("pidfd unavailable: %v", err)
	}
	_ = unix.Close(fd)
	if err := os.WriteFile(filepath.Join(dir, "stdout"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{}, 1)
	result := make(chan error, 1)
	go func() {
		_, err := Observe(ctx, dir, id, writerFunc(func(p []byte) (int, error) {
			ready <- struct{}{}
			return len(p), nil
		}), io.Discard)
		result <- err
	}()
	receive(t, ready)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := receive(t, result); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("live wrapper death: %v", err)
	}
}

func TestEventWaiterReadOnlyAndCancellation(t *testing.T) {
	dir, id := liveFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	w := newEventWaiter(ctx, dir, id, id.Boot, observationBackup, true)
	defer w.close()
	// Observation reads must not create an endless cycle of self-wakeups.
	if _, err := readMetadata(filepath.Join(dir, "stdout")); err != nil {
		t.Fatal(err)
	}
	for wakes := 0; ; wakes++ {
		err := w.wait(ctx)
		if errors.Is(err, context.DeadlineExceeded) {
			break
		}
		// A signal can interrupt epoll_wait. Permit bounded spurious wakes,
		// but not the continuous readiness caused by watching our own reads.
		if err != nil || wakes >= 8 {
			t.Fatalf("read generated wakeups or cancellation failed: %v", err)
		}
	}
}

func TestObserveCancellationWhileIdle(t *testing.T) {
	dir, id := liveFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := Observe(ctx, dir, id, io.Discard, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("idle cancellation: %v after %v", err, time.Since(started))
	}
}

func TestObservePollingFallback(t *testing.T) {
	dir, id := liveFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{}, 1)
	result := make(chan error, 1)
	if err := os.WriteFile(filepath.Join(dir, "stdout"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, err := observe(ctx, dir, id, writerFunc(func(p []byte) (int, error) {
			ready <- struct{}{}
			return len(p), nil
		}), io.Discard, 80*time.Millisecond, false)
		result <- err
	}()
	receive(t, ready)
	publishCompletion(t, dir, id)
	if err := receive(t, result); err != nil {
		t.Fatal(err)
	}
}

func TestEventWaiterRejectsReusedPID(t *testing.T) {
	dir, id := liveFixture(t)
	id.Start += "0"
	w := newEventWaiter(context.Background(), dir, id, id.Boot, observationBackup, true)
	defer w.close()
	if w.pidfd >= 0 {
		t.Fatal("pidfd accepted for a different process identity")
	}
}

func TestPidfdReadinessConsumedOnce(t *testing.T) {
	cmd, dir, id, _ := wrapper(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	w := newEventWaiter(ctx, dir, id, id.Boot, observationBackup, true)
	defer w.close()
	if w.pidfd < 0 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Skip("pidfd unavailable")
	}
	// Remove inotify to isolate persistent pidfd readiness.
	w.disable(&w.inotify)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if err := w.wait(ctx); err != nil {
		t.Fatal(err)
	}
	if w.pidfd >= 0 {
		t.Fatal("terminal pidfd remained armed")
	}
	cancel()
	if err := w.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel after death: %v", err)
	}
}

func TestEventDrainOverflowAndIgnored(t *testing.T) {
	// A nonblocking pipe supplies exact kernel event records without relying
	// on host inotify queue limits or changing global sysctls.
	var pipe [2]int
	if err := unix.Pipe2(pipe[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pipe[1])
	w := &eventWaiter{inotify: pipe[0], pidfd: -1}
	defer w.close()
	for _, mask := range []uint32{unix.IN_Q_OVERFLOW, unix.IN_IGNORED} {
		var event [unix.SizeofInotifyEvent]byte
		binary.NativeEndian.PutUint32(event[4:], mask)
		if _, err := unix.Write(pipe[1], event[:]); err != nil {
			t.Fatal(err)
		}
		w.drainEvents()
		if mask == unix.IN_Q_OVERFLOW && w.inotify < 0 {
			t.Fatal("overflow disabled recoverable event source")
		}
	}
	if w.inotify >= 0 {
		t.Fatal("ignored watch left armed")
	}
}

func TestEventWaiterBackupWithLivePidfd(t *testing.T) {
	dir, id := liveFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	w := newEventWaiter(ctx, dir, id, id.Boot, 40*time.Millisecond, true)
	defer w.close()
	if w.pidfd < 0 {
		t.Skip("pidfd unavailable")
	}
	w.disable(&w.inotify)
	started := time.Now()
	if err := w.wait(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("quiet live pidfd did not use backup: %v", elapsed)
	}
	if !w.nextBackup.After(time.Now()) {
		t.Fatal("runtime-poller timeout left backup expired, causing a duplicate scan")
	}
}

func TestEventWaiterFallbackAdvancesBackup(t *testing.T) {
	w := newEventWaiter(context.Background(), "", Identity{}, "", 20*time.Millisecond, false)
	defer w.close()
	if err := w.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !w.nextBackup.After(time.Now()) {
		t.Fatal("fallback timeout left backup expired, causing a duplicate scan")
	}
	// A deadline already expired when entering wait still requests a scan
	// immediately, rather than deferring it by another backup interval.
	w.nextBackup = time.Now().Add(-time.Second)
	started := time.Now()
	if err := w.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) >= 10*time.Millisecond || !w.nextBackup.After(time.Now()) {
		t.Fatal("expired backup did not immediately advance and request a scan")
	}
}

func TestEventWaiterCancellationCleanup(t *testing.T) {
	dir, id := liveFixture(t)
	// Warm the Go runtime poller before taking the descriptor baseline.
	w := newEventWaiter(context.Background(), dir, id, id.Boot, observationBackup, true)
	w.close()
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		ctx, cancel := context.WithCancel(context.Background())
		if i%2 == 0 {
			cancel() // Exercise cancellation racing waiter construction.
		}
		w := newEventWaiter(ctx, dir, id, id.Boot, observationBackup, true)
		cancel()
		if err := w.wait(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
		w.close()
	}
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) > len(before) {
		t.Fatalf("descriptor leak: before=%d after=%d", len(before), len(after))
	}
}
