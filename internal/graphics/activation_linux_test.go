package graphics

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTmuxActivationNotifications(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"%session-window-changed $2 @3", true},
		{"%session-window-changed $2 @4", false},
		{"%session-window-changed $1 @3", false},
		{"%client-session-changed /dev/pts/8 $2 research", true},
		{"%client-session-changed /dev/pts/8 $1 other", false},
		{"%output %4 image bytes", false},
		{"%session-changed $2 research", false},
		{"%session-window-changed $2", false},
	} {
		if got := tmuxActivation(tc.line, "$2", "@3"); got != tc.want {
			t.Fatalf("%q: %v", tc.line, got)
		}
	}
}

func TestTmuxActivationLifecycle(t *testing.T) {
	dir := tmuxFixture(t, `if [ "$1" = display-message ]; then
 printf '$2 @3\n'
 exit
fi
printf '%s\n' "$@" > "$TTC_ACTIVATION_ARGS"
printf '%%session-window-changed $2 @4\n%%session-window-changed $2 @3\n'
exec sleep 30
`)
	args := filepath.Join(dir, "args")
	t.Setenv("TTC_ACTIVATION_ARGS", args)
	events, stop, err := WatchTmuxActivation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	select {
	case _, ok := <-events:
		if !ok {
			t.Fatal("observer exited")
		}
	case <-time.After(time.Second):
		t.Fatal("missed window return")
	}
	got, err := os.ReadFile(args)
	if err != nil || string(got) != "-C\nattach-session\n-E\n-f\nno-output,ignore-size\n-t\n$2\n" {
		t.Fatal("observer must preserve the session environment and sizing and suppress pane output", string(got), err)
	}
	start := time.Now()
	stop()
	if time.Since(start) > time.Second {
		t.Fatal("observer did not join promptly")
	}
	if _, ok := <-events; ok {
		t.Fatal("observer events remain open after stop")
	}
}

func TestTmuxActivationRejectsMissingTarget(t *testing.T) {
	t.Setenv("TMUX_PANE", "")
	if _, _, err := WatchTmuxActivation(context.Background()); err == nil {
		t.Fatal("missing pane accepted")
	}
	tmuxFixture(t, "printf 'invalid\\n'\n")
	if _, _, err := WatchTmuxActivation(context.Background()); err == nil {
		t.Fatal("invalid target accepted")
	}
}
