package assets

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"os/exec"
	"strings"
	"time"
)

// MathRenderer owns one pre-warmed MathJax process. Render and Close must be
// called by one owner goroutine; canceling its parent context kills the process.
// Each formula has fresh parser state, bounded output, and a ten-second deadline.
type MathRenderer struct {
	ctx    context.Context
	cancel context.CancelFunc
	root   string
	cmd    *exec.Cmd
	stop   context.CancelFunc
	input  io.WriteCloser
	output *bufio.Scanner
	stderr limitedBuffer
}

// NewMathRenderer installs the pinned package if necessary and pre-warms Node.
// Initialization is bounded to two minutes. Close it before releasing its owner.
func NewMathRenderer(ctx context.Context) (*MathRenderer, error) {
	setup, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	root, err := ensureMath(setup)
	if err != nil {
		return nil, err
	}
	owned, stop := context.WithCancel(ctx)
	r := &MathRenderer{ctx: owned, cancel: stop, root: root}
	if err = r.start(setup); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// Version identifies the immutable dependency lock and rendering code.
func (r *MathRenderer) Version() string {
	return "mathjax-" + mathjaxVersion + ":" + mathPackageKey + ":" + Key(mathBackend)
}

// Close cancels and joins the managed process. Repeated calls are harmless.
func (r *MathRenderer) Close() { r.cancel(); r.finish() }

func (r *MathRenderer) start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	process, cancel := context.WithCancel(r.ctx)
	cmd := exec.CommandContext(process, "node", "--max-old-space-size=128", "--input-type=commonjs", "--eval", mathBackend, r.root, "worker")
	boundProcess(cmd)
	cmd.Env = mathEnvironment()
	r.stderr = limitedBuffer{limit: 4096}
	cmd.Stderr = &r.stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		cancel()
		return err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		return err
	}
	r.cmd, r.stop, r.input = cmd, cancel, input
	r.output = bufio.NewScanner(output)
	r.output.Buffer(make([]byte, 64<<10), MaxBytes+1)
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	reply, err := r.read()
	if err != nil || !reply.Ready || reply.SVG != "" || reply.Error != "" {
		if err == nil {
			err = fmt.Errorf("invalid MathJax startup reply")
		}
		return r.failed(ctx, err)
	}
	return nil
}

type mathReply struct {
	Ready bool   `json:"ready"`
	SVG   string `json:"svg"`
	Error string `json:"error"`
}

func (r *MathRenderer) read() (mathReply, error) {
	if !r.output.Scan() {
		if err := r.output.Err(); err != nil {
			return mathReply{}, err
		}
		return mathReply{}, io.EOF
	}
	var reply mathReply
	err := json.Unmarshal(r.output.Bytes(), &reply)
	return reply, err
}

// Render typesets one formula using the warm engine and converts its SVG to PNG.
// Cancellation interrupts the current engine; the next render starts a new one.
// Unsupported TeX is an ordinary error and leaves the engine available.
func (r *MathRenderer) Render(ctx context.Context, tex string, pixels int) (image.Image, error) {
	if len(tex) == 0 || len(tex) > 4096 {
		return nil, fmt.Errorf("formula requires 1–4096 bytes")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.cmd == nil {
		if err := r.start(ctx); err != nil {
			return nil, err
		}
	}
	stop := context.AfterFunc(ctx, r.stop)
	defer stop()
	request := struct {
		TeX    string `json:"tex"`
		Pixels int    `json:"pixels"`
	}{tex, max(8, min(pixels, 128))}
	data, err := json.Marshal(request)
	if err == nil {
		_, err = r.input.Write(append(data, '\n'))
	}
	if err != nil {
		return nil, r.failed(ctx, err)
	}
	reply, err := r.read()
	if err != nil {
		return nil, r.failed(ctx, err)
	}
	if reply.Ready || (reply.Error == "") == (reply.SVG == "") || len(reply.Error) > 16<<10 {
		return nil, r.failed(ctx, fmt.Errorf("invalid MathJax render reply"))
	}
	if reply.Error != "" {
		return nil, fmt.Errorf("MathJax: %s", reply.Error)
	}
	png, err := renderOutput(ctx, exec.CommandContext(ctx, "rsvg-convert"), []byte(reply.SVG), MaxBytes)
	if err != nil {
		if ctx.Err() != nil {
			r.finish()
		}
		return nil, fmt.Errorf("SVG conversion: %w", err)
	}
	return Decode(png)
}

func (r *MathRenderer) failed(ctx context.Context, err error) error {
	r.finish()
	if ctx.Err() != nil {
		err = ctx.Err()
	} else if r.ctx.Err() != nil {
		err = r.ctx.Err()
	}
	return fmt.Errorf("MathJax worker: %w: %s", err, strings.TrimSpace(r.stderr.buffer.String()))
}

func (r *MathRenderer) finish() {
	if r.cmd != nil {
		r.stop()
		r.input.Close()
		_ = r.cmd.Wait() // Process termination is intentional; diagnostics stay in stderr.
		r.cmd, r.input, r.output, r.stop = nil, nil, nil, nil
	}
}
