package tui

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v3"
	"github.com/gdamore/tcell/v3/tty"
)

// headlessTTY backs screen tests that inject events through Screen.EventQ and
// inspect Screen.Get, rather than terminal bytes. Protocol handling is covered
// by the PTY tests. Unlike vt.MockTerm, its lifecycle state is synchronized with
// Read: tcell can resume before the previous input loop's reader has returned.
type headlessTTY struct {
	mu      sync.Mutex
	size    tty.WindowSize
	stopped chan struct{} // closed to wake readers; nil before Start or after Stop
}

func newHeadlessTTY(width, height int) *headlessTTY {
	return &headlessTTY{size: tty.WindowSize{Width: width, Height: height}}
}

func (t *headlessTTY) Start() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped == nil {
		t.stopped = make(chan struct{})
	}
	return nil
}

func (t *headlessTTY) Stop() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped != nil {
		close(t.stopped)
		t.stopped = nil
	}
	return nil
}

func (t *headlessTTY) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	t.mu.Lock()
	stopped := t.stopped
	t.mu.Unlock()
	if stopped != nil {
		<-stopped
	}
	return 0, io.EOF
}

func (t *headlessTTY) Write(p []byte) (int, error) { return len(p), nil }
func (t *headlessTTY) Close() error                { return t.Stop() }

// Drain wakes readers without affecting writes of terminal teardown sequences.
func (t *headlessTTY) Drain() error { return t.Stop() }

// Tests post resize events explicitly after changing the dimensions.
func (t *headlessTTY) NotifyResize(chan<- bool) {}

func (t *headlessTTY) WindowSize() (tty.WindowSize, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.size, nil
}

func (t *headlessTTY) setSize(width, height int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.size = tty.WindowSize{Width: width, Height: height}
}

func TestHeadlessTTYLifecycle(t *testing.T) {
	term := newHeadlessTTY(80, 25)
	t.Cleanup(func() { _ = term.Close() })
	for range 100 {
		if err := term.Start(); err != nil {
			t.Fatal(err)
		}
		// A reader snapshots this generation. Restart must not reopen it or
		// redirect its wait to the next generation, even if it returns late.
		old := term.stopped
		done := make(chan error, 1)
		go func() {
			_, err := term.Read(make([]byte, 1))
			done <- err
		}()
		if err := term.Start(); err != nil {
			t.Fatal(err)
		}
		if term.stopped != old {
			t.Fatal("repeated Start replaced the read generation")
		}
		if err := term.Drain(); err != nil {
			t.Fatal(err)
		}
		if n, err := term.Write([]byte("teardown")); n != 8 || err != nil {
			t.Fatalf("Write after Drain = %d, %v", n, err)
		}
		if err := term.Stop(); err != nil {
			t.Fatal(err)
		}
		if err := term.Start(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-old:
		default:
			t.Fatal("restart left the previous read generation blocked")
		}
		select {
		case <-term.stopped:
			t.Fatal("restart did not create an open read generation")
		default:
		}
		// The goroutine may not have entered Read until after the restart;
		// stop that generation too before joining it.
		if err := term.Stop(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != io.EOF {
				t.Fatalf("Read = %v, want EOF", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Drain/Stop did not unblock Read")
		}
	}
}

func TestScreenSuspendResume(t *testing.T) {
	screen := newTestScreen(t)
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	for i := range 100 {
		if err := screen.Suspend(); err != nil {
			t.Fatal(err)
		}
		width, height := 80+i%2, 25+i%2
		screen.term.setSize(width, height)
		if err := screen.Resume(); err != nil {
			t.Fatal(err)
		}
		// The editor workflow syncs after resume to refresh terminal geometry.
		screen.Sync()
		if w, h := screen.Size(); w != width || h != height {
			t.Fatalf("size after resume = %dx%d, want %dx%d", w, h, width, height)
		}
		screen.Put(0, 0, "x", tcell.StyleDefault)
		screen.Show()
		if text, _, _ := screen.Get(0, 0); text != "x" {
			t.Fatalf("cell after resume = %q", text)
		}
	}
}
