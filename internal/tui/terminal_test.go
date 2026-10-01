package tui

import (
	"io"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
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
