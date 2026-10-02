package graphics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTmuxGraphicsMetadata(t *testing.T) {
	for _, test := range []struct{ metadata, want string }{
		{"kitty(0.49.1)\non\nbpaste,RGB,title\n", ""},
		{"kitty(0.49.1)\nall\nRGB\n", ""},
		{"kitty(0.49.1)\noff\nRGB\n", "passthrough is disabled"},
		{"\non\nRGB\n", "did not identify"},
		{"xterm(400)\non\nRGB\n", "did not identify"},
		{"not-kitty(1)\non\nRGB\n", "did not identify"},
		{"kitty(0.49.1)\non\nnotRGB\n", "lacks RGB"},
		{"kitty(0.49.1)\non\n", "invalid"},
		{"kitty(0.49.1)\non\nRGB\nextra\n", "invalid"},
	} {
		err := tmuxGraphics(test.metadata)
		if test.want == "" && err != nil || test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
			t.Fatalf("%q: %v, want %q", test.metadata, err, test.want)
		}
	}
}

func tmuxFixture(t *testing.T, command string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+command), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("TMUX_PANE", "%21")
	return dir
}

func TestTmuxProbeNeedsNoTerminalReplyAndUsesEffectivePane(t *testing.T) {
	dir := tmuxFixture(t, `printf '%s\n' "$@" > "$TTC_PROBE_ARGS"
printf 'kitty(0.49.1)\non\nbpaste,RGB,title\n'
`)
	args := filepath.Join(dir, "args")
	t.Setenv("TTC_PROBE_ARGS", args)
	t.Setenv("KITTY_WINDOW_ID", "")
	t.Setenv("TERM", "tmux-256color")
	t.Setenv("COLORTERM", "")
	enabled, pending, err := Probe(context.Background(), true)
	if err != nil || !enabled || len(pending) != 0 {
		t.Fatal("Kitty with pane passthrough should not need an APC reply", enabled, pending, err)
	}
	got, err := os.ReadFile(args)
	want := "display-message\n-p\n-t\n%21\n#{client_termtype}\n#{allow-passthrough}\n#{client_termfeatures}\n"
	if err != nil || string(got) != want {
		t.Fatal("query did not select effective pane metadata", string(got), err)
	}
}

func TestTmuxProbeBoundsOutputAndPreservesCancellation(t *testing.T) {
	t.Run("output", func(t *testing.T) {
		tmuxFixture(t, "head -c 5000 /dev/zero\n")
		if err := probeTmux(context.Background()); err == nil || !strings.Contains(err.Error(), "exceeds 4 KiB") {
			t.Fatal("unbounded metadata accepted", err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		tmuxFixture(t, "sleep 30\n")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		start := time.Now()
		if err := probeTmux(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
			t.Fatal("metadata command ignored cancellation", err, time.Since(start))
		}
	})
	t.Run("failure", func(t *testing.T) {
		tmuxFixture(t, "printf 'error connecting to socket: Permission denied\\n' >&2\nexit 1\n")
		if err := probeTmux(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot read tmux") || !strings.Contains(err.Error(), "Permission denied") {
			t.Fatal("inaccessible tmux treated as graphics support", err)
		}
	})
	t.Run("stderr bound", func(t *testing.T) {
		tmuxFixture(t, "head -c 5000 /dev/zero >&2\nexit 1\n")
		err := probeTmux(context.Background())
		// Quoting expands each NUL to four printable bytes; only 1 KiB may
		// be retained, and raw terminal controls must never enter a warning.
		if err == nil || len(err.Error()) > 4300 || strings.ContainsRune(err.Error(), 0) {
			t.Fatal("unbounded or unsafe tmux error output", err)
		}
	})
}
