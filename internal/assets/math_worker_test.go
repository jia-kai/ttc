package assets

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWarmRendererRecoveryCancellationAndClose(t *testing.T) {
	fakeMathInstall(t)
	r, err := NewMathRenderer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if version := r.Version(); !strings.Contains(version, mathjaxVersion) || !strings.Contains(version, mathPackageKey) {
		t.Fatal("renderer version omitted the pinned dependency", version)
	}
	firstPID := r.cmd.Process.Pid
	for _, tex := range []string{"", strings.Repeat("x", 4097)} {
		if _, err := r.Render(context.Background(), tex, 16); err == nil {
			t.Fatal("accepted formula outside source bounds", len(tex))
		}
		if r.cmd.Process.Pid != firstPID {
			t.Fatal("invalid source destroyed the warm process")
		}
	}
	for range 10 {
		if _, err := r.Render(context.Background(), "x^2", 16); err != nil {
			t.Fatal(err)
		}
		if r.cmd.Process.Pid != firstPID {
			t.Fatal("warm process was recreated for a formula")
		}
	}
	if _, err := r.Render(context.Background(), "unsupported", 16); err == nil || !strings.Contains(err.Error(), "unsupported fixture") {
		t.Fatal("unsupported input was hidden", err)
	}
	if r.cmd.Process.Pid != firstPID {
		t.Fatal("ordinary math error destroyed warm process")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.Render(ctx, "stall", 16); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("warm renderer ignored cancellation", err)
	}
	if r.cmd != nil {
		t.Fatal("interrupted worker was retained")
	}
	if _, err := r.Render(context.Background(), "x", 16); err != nil {
		t.Fatal("worker did not recover", err)
	}
	if _, err := r.Render(context.Background(), "malformed", 16); err == nil {
		t.Fatal("invalid frame was accepted")
	}
	if _, err := r.Render(context.Background(), "x", 16); err != nil {
		t.Fatal("worker did not recover from invalid frame", err)
	}
	r.Close()
	r.Close()
	if _, err := r.Render(context.Background(), "x", 16); err == nil {
		t.Fatal("closed renderer resumed")
	}
}

func TestWarmMathParserIsolation(t *testing.T) {
	isolatedMathCache(t)
	r, err := NewMathRenderer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	pid := r.cmd.Process.Pid
	if _, err := r.Render(context.Background(), `\newcommand{\ttcmacro}{x}\ttcmacro`, 16); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Render(context.Background(), `\ttcmacro`, 16); err == nil {
		t.Fatal("macro state leaked into later formula")
	}
	if _, err := r.Render(context.Background(), `\begin{pmatrix}1&2\\3&4\end{pmatrix}`, 16); err != nil {
		t.Fatal(err)
	}
	if r.cmd.Process.Pid != pid {
		t.Fatal("real engine did not remain warm")
	}
}
