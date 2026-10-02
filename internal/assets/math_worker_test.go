package assets

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMathRasterResolutionPreservesLogicalSize(t *testing.T) {
	isolatedMathCache(t)
	r, err := NewMathRenderer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i, tex := range []string{`x^2`, `\frac{1}{n}`, `\begin{pmatrix}1&2\\3&4\end{pmatrix}`} {
		cmd := exec.CommandContext(ctx, "node", "--input-type=commonjs", "--eval", mathBackend, r.root, "16", "1")
		cmd.Env = mathEnvironment()
		svg, err := renderOutput(ctx, cmd, []byte(tex), MaxBytes)
		if err != nil {
			t.Fatal(err)
		}
		var canvas struct {
			Width   string `xml:"width,attr"`
			Height  string `xml:"height,attr"`
			ViewBox string `xml:"viewBox,attr"`
		}
		if err := xml.Unmarshal(svg, &canvas); err != nil {
			t.Fatal(err)
		}
		var x, y, width, height float64
		if _, err := fmt.Sscan(canvas.ViewBox, &x, &y, &width, &height); err != nil {
			t.Fatal(err)
		}
		pw, _ := strconv.ParseFloat(canvas.Width, 64)
		ph, _ := strconv.ParseFloat(canvas.Height, 64)
		if math.Abs(pw/width-16.0/1000) > 1e-9 || math.Abs(ph/height-16.0/1000) > 1e-9 {
			t.Fatal("SVG padding changed em scale or aspect ratio", canvas)
		}
		encoded, err := renderOutput(ctx, exec.CommandContext(ctx, "rsvg-convert"), svg, MaxBytes)
		if err != nil {
			t.Fatal(err)
		}
		base, err := Decode(encoded)
		if err != nil {
			t.Fatal(err)
		}
		high, err := r.Render(ctx, tex, 16)
		if err != nil {
			t.Fatal(err)
		}
		if high.Bounds().Dx() != MathRasterScale*base.Bounds().Dx() || high.Bounds().Dy() != MathRasterScale*base.Bounds().Dy() {
			t.Fatalf("formula %q: %v -> %v", tex, base.Bounds(), high.Bounds())
		}
		if i == 0 {
			setup, err := formulaAt(ctx, r.root, tex, 16)
			if err != nil || setup.Bounds() != high.Bounds() {
				t.Fatal("setup renderer disagrees with warm raster resolution", err)
			}
		}
		// An explicit directory saves real PNGs for visual inspection without
		// making tests depend on machine-local paths.
		if dir := os.Getenv("TTC_MATH_TEST_ARTIFACTS"); dir != "" {
			file, err := os.Create(filepath.Join(dir, fmt.Sprintf("formula-%d-3x.png", i)))
			if err != nil {
				t.Fatal(err)
			}
			err = png.Encode(file, high)
			closeErr := file.Close()
			if err != nil || closeErr != nil {
				t.Fatal(err, closeErr)
			}
		}
	}
	// This canvas fits the former logical bounds but not the supersampled
	// physical pixel budget. Reject it before asking librsvg to allocate it.
	if _, err := r.Render(ctx, `\rule{31em}{6em}`, 128); err == nil || !strings.Contains(err.Error(), "dimensions exceed render limits") {
		t.Fatal("supersampling bypassed physical pixel bounds", err)
	}
}

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
