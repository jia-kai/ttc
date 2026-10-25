package jobs

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ttc/internal/capture"
)

func TestForegroundBudgetPromotesSameProcessAndPreservesDeadline(t *testing.T) {
	notices := make(chan Snapshot, 2)
	m := New(context.Background(), func(v Snapshot) { notices <- v })
	defer m.Close()
	id, err := m.Start("main", "printf ready; sleep 30", t.TempDir(), 200*time.Millisecond, true, false, capture.OutputOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	process := m.jobs[id].cmd.Process
	deadline, _ := m.jobs[id].ctx.Deadline()
	m.mu.Unlock()
	v, err := m.WaitForeground(context.Background(), "main", id, 5*time.Millisecond, nil)
	if err != nil || v.Status != "running" || !v.Promoted || !v.Background || v.ForegroundWaitSeconds == nil || *v.ForegroundWaitSeconds != 0.005 {
		t.Fatal(v, err)
	}
	m.mu.Lock()
	after := m.jobs[id].cmd.Process
	afterDeadline, _ := m.jobs[id].ctx.Deadline()
	pendingResult := m.jobs[id].foregroundResultPending
	m.mu.Unlock()
	if pendingResult {
		t.Fatal("consumed handoff retained original result ownership")
	}
	if process != after || !deadline.Equal(afterDeadline) {
		t.Fatal("promotion restarted process or deadline")
	}
	select {
	case v = <-notices:
		if v.CancellationReason != "execution_deadline" || v.Status != "cancelled" {
			t.Fatal(v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deadline did not terminate promoted process")
	}
	m.wg.Wait()
	select {
	case v = <-notices:
		t.Fatal("duplicate completion", v)
	default:
	}
}

func TestCancellationReasons(t *testing.T) {
	for _, reason := range []string{"execution_deadline", "explicit_stop", "caller_cancelled", "runtime_cleanup"} {
		t.Run(reason, func(t *testing.T) {
			notices := make(chan Snapshot, 1)
			m := New(context.Background(), func(v Snapshot) { notices <- v })
			defer m.Close()
			timeout := time.Duration(0)
			if reason == "execution_deadline" {
				timeout = 5 * time.Millisecond
			}
			background := reason != "caller_cancelled"
			id, err := m.Start("main", "sleep 30", t.TempDir(), timeout, true, background, capture.OutputOptions{}, "")
			if err != nil {
				t.Fatal(err)
			}
			var v Snapshot
			switch reason {
			case "explicit_stop":
				v, err = m.Stop("main", id)
			case "caller_cancelled":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				v, err = m.Wait(ctx, "main", id, nil)
			case "runtime_cleanup":
				m.StopOwned("main")
				v, err = m.View("main", id)
			default:
				v, err = m.Wait(context.Background(), "main", id, nil)
			}
			if err != nil || v.Status != "cancelled" || v.CancellationReason != reason {
				t.Fatal(v, err)
			}
			m.wg.Wait()
			if background {
				select {
				case n := <-notices:
					if n.CancellationReason != reason {
						t.Fatal(n)
					}
				default:
					t.Fatal("background completion missing")
				}
			}
			select {
			case n := <-notices:
				t.Fatal("unexpected notification", n)
			default:
			}
		})
	}
}

func TestCompletionPromotionRaceHasOneTerminalDelivery(t *testing.T) {
	for i := 0; i < 100; i++ {
		var mu sync.Mutex
		var notices []Snapshot
		m := New(context.Background(), func(v Snapshot) { mu.Lock(); notices = append(notices, v); mu.Unlock() })
		id, err := m.Start("main", "true", t.TempDir(), time.Second, true, false, capture.OutputOptions{}, "")
		if err != nil {
			t.Fatal(err)
		}
		v, err := m.WaitForeground(context.Background(), "main", id, time.Microsecond, nil)
		if err != nil {
			t.Fatal(err)
		}
		m.wg.Wait()
		mu.Lock()
		count := len(notices)
		mu.Unlock()
		if v.Status == "running" {
			if !v.Promoted || count != 1 {
				t.Fatal(v, count)
			}
		} else if v.Promoted || count != 0 {
			t.Fatal("terminal tool result duplicated a background notice", v, count)
		}
		m.Close()
	}
}

func TestCloseReportsCleanupReason(t *testing.T) {
	notices := make(chan Snapshot, 1)
	m := New(context.Background(), func(v Snapshot) { notices <- v })
	_, err := m.Start("main", "sleep 30", t.TempDir(), 0, true, true, capture.OutputOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	select {
	case v := <-notices:
		if v.CancellationReason != "runtime_cleanup" {
			t.Fatal(v)
		}
	default:
		t.Fatal("cleanup diagnostic notification missing")
	}
}

func TestUserPromotionWinsTerminalAndCallerCancellationRace(t *testing.T) {
	notices := make(chan Snapshot, 2)
	m := New(context.Background(), func(v Snapshot) { notices <- v })
	defer m.Close()
	id, err := m.Start("main", "sleep 30", t.TempDir(), time.Second, true, false, capture.OutputOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan Snapshot, 1)
	go func() {
		v, err := m.WaitForeground(ctx, "main", id, 5*time.Millisecond, func(Snapshot) { close(entered); <-release })
		if err != nil {
			t.Error(err)
		}
		result <- v
	}()
	<-entered
	if _, err := m.Promote("main", id); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stop("main", id); err != nil {
		t.Fatal(err)
	}
	cancel()
	close(release)
	select {
	case v := <-result:
		if v.Status != "running" || !v.Promoted {
			t.Fatal("handoff leaked terminal result", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiter did not return")
	}
	m.wg.Wait()
	select {
	case <-notices:
	default:
		t.Fatal("completion missing")
	}
	select {
	case v := <-notices:
		t.Fatal("duplicate completion", v)
	default:
	}
}

func TestCancellationBeforePromotionKeepsForegroundTerminalOwnership(t *testing.T) {
	notices := make(chan Snapshot, 1)
	m := New(context.Background(), func(v Snapshot) { notices <- v })
	defer m.Close()
	id, err := m.Start("main", "sleep 30", t.TempDir(), 0, true, false, capture.OutputOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	cancelJob(m.jobs[id], "caller_cancelled")
	m.mu.Unlock()
	v, err := m.Promote("main", id)
	if err != nil || v.Status != "cancelled" || v.Promoted || v.CancellationReason != "caller_cancelled" {
		t.Fatal(v, err)
	}
	m.wg.Wait()
	select {
	case v := <-notices:
		t.Fatal("foreground terminal notified as background", v)
	default:
	}
}

func TestPromotionAndCompletionBeforeForegroundWaitKeepsOneTerminalDelivery(t *testing.T) {
	notices := make(chan Snapshot, 2)
	m := New(context.Background(), func(v Snapshot) { notices <- v })
	defer m.Close()
	root := t.TempDir()
	id, err := m.Start("main", "while [ ! -e release ]; do sleep 0.01; done; printf done; exit 7", root, time.Second, true, false, capture.OutputOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	// The shell tool can be preempted between Start and waiter registration.
	// Complete the entire promotion/publication sequence in that window.
	if _, err := m.Promote("main", id); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	m.wg.Wait()
	select {
	case n := <-notices:
		if n.Status != "completed" || n.ExitCode == nil || *n.ExitCode != 7 {
			t.Fatal(n)
		}
	default:
		t.Fatal("background terminal notification missing")
	}
	v, err := m.WaitForeground(context.Background(), "main", id, time.Second, nil)
	if err != nil || v.Status != "running" || !v.Promoted || !v.Background || v.ExitCode != nil || v.FinishedAt != "" || v.Signal != "" || v.CancellationReason != "" {
		t.Fatal("original foreground result duplicated terminal notification", v, err)
	}
	if v.ForegroundWaitSeconds == nil || *v.ForegroundWaitSeconds != 1 {
		t.Fatal("handoff lost effective waiter budget", v)
	}
	// A later explicit observation still receives the real retained terminal state.
	v, err = m.Wait(context.Background(), "main", id, nil)
	if err != nil || v.Status != "completed" || v.ExitCode == nil || *v.ExitCode != 7 || v.Stdout != "done" {
		t.Fatal(v, err)
	}
	select {
	case n := <-notices:
		t.Fatal("duplicate terminal notification", n)
	default:
	}
}
