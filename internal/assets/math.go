package assets

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const mathjaxVersion = "4.1.3"

//go:embed mathjax_backend.cjs
var mathBackend string

// mathRoot locates a versioned optional package in the user's XDG cache.
func mathRoot() (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve dependency cache: %w", err)
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("dependency cache must be absolute: %s", root)
	}
	return filepath.Join(root, "ttc", "mathjax", mathjaxVersion+"-"+mathPackageKey[:12]), nil
}

func checkMath(ctx context.Context, root string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, name := range []string{"node", "rsvg-convert"} {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("%s is required: %w", name, err)
		}
	}
	for _, name := range []string{"src", "mathjax-newcm-font"} {
		path := filepath.Join(root, "node_modules", "@mathjax", name, "package.json")
		f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return fmt.Errorf("read MathJax package %s: %w", path, err)
		}
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() || st.Size() > 64<<10 {
			f.Close()
			return fmt.Errorf("invalid MathJax package metadata: %s", path)
		}
		data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
		f.Close()
		if err != nil || len(data) > 64<<10 {
			return fmt.Errorf("cannot read bounded MathJax metadata: %s", path)
		}
		var pkg struct{ Name, Version string }
		if err = json.Unmarshal(data, &pkg); err != nil {
			return fmt.Errorf("decode MathJax package %s: %w", path, err)
		}
		if pkg.Name != "@mathjax/"+name || pkg.Version != mathjaxVersion {
			return fmt.Errorf("require @mathjax/%s@%s; found %s@%s", name, mathjaxVersion, pkg.Name, pkg.Version)
		}
	}
	return nil
}

// formulaAt validates a staged installation before publishing its cache marker.
func formulaAt(ctx context.Context, root, tex string, pixels int) (image.Image, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--input-type=commonjs", "--eval", mathBackend, root, fmt.Sprint(max(8, min(pixels, 128))))
	cmd.Env = mathEnvironment()
	svg, err := renderOutput(ctx, cmd, []byte(tex), MaxBytes)
	if err != nil {
		return nil, fmt.Errorf("MathJax: %w", err)
	}
	png, err := renderOutput(ctx, exec.CommandContext(ctx, "rsvg-convert"), svg, MaxBytes)
	if err != nil {
		return nil, fmt.Errorf("SVG conversion: %w", err)
	}
	return Decode(png)
}

func mathEnvironment() []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name != "NODE_OPTIONS" && name != "NODE_PATH" {
			env = append(env, entry)
		}
	}
	return env
}

func renderOutput(ctx context.Context, cmd *exec.Cmd, input []byte, limit int) ([]byte, error) {
	boundProcess(cmd)
	cmd.Stdin = bytes.NewReader(input)
	out, stderr := limitedBuffer{limit: limit}, limitedBuffer{limit: 4096}
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.buffer.String()))
	}
	return out.buffer.Bytes(), nil
}

type limitedBuffer struct {
	buffer bytes.Buffer // Keep io.Copy on Write; embedding promotes unbounded ReadFrom.
	limit  int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	available := max(0, b.limit-b.buffer.Len())
	if available > 0 {
		_, _ = b.buffer.Write(p[:min(n, available)])
	}
	if n > available {
		return n, fmt.Errorf("renderer output limit exceeded")
	}
	return n, nil
}

func boundProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
}
