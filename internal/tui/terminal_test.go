package tui

import (
	"io"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v3"
)

type inputTTY struct {
	tcell.Tty
	input io.Reader
	reads int
}

func (t *inputTTY) Read(b []byte) (int, error) { t.reads++; return t.input.Read(b) }

func TestProbeInputIsDeliveredBeforeLiveTTY(t *testing.T) {
	live := &inputTTY{input: strings.NewReader("live")}
	tty := &terminalTTY{Tty: live, pending: []byte("keys")}
	b := make([]byte, 2)
	for _, want := range []string{"ke", "ys", "li"} {
		n, err := tty.Read(b)
		if err != nil || string(b[:n]) != want {
			t.Fatal(string(b[:n]), err)
		}
		if want != "li" && live.reads != 0 {
			t.Fatal("live input read before pending input")
		}
	}
	if live.reads != 1 || len(tty.pending) != 0 {
		t.Fatal("incorrect handoff", live.reads, tty.pending)
	}
}

type blockedWriteTTY struct {
	tcell.Tty
	entered chan struct{}
	release chan struct{}
}

func (t *blockedWriteTTY) Write(b []byte) (int, error) {
	close(t.entered)
	<-t.release
	return len(b), io.ErrClosedPipe
}

func TestTerminalWriteHoldsSharedLock(t *testing.T) {
	live := &blockedWriteTTY{entered: make(chan struct{}), release: make(chan struct{})}
	tty := &terminalTTY{Tty: live}
	done := make(chan struct{})
	var n int
	var err error
	go func() {
		n, err = tty.Write([]byte("protocol"))
		close(done)
	}()
	<-live.entered
	locked := !tty.mu.TryLock()
	if !locked {
		tty.mu.Unlock()
	}
	close(live.release)
	<-done
	if !locked {
		t.Fatal("underlying write was not protected by the shared lock")
	}
	if n != len("protocol") || err != io.ErrClosedPipe {
		t.Fatal("write result was not preserved", n, err)
	}
	if !tty.mu.TryLock() {
		t.Fatal("write error left the shared lock held")
	}
	tty.mu.Unlock()
}
