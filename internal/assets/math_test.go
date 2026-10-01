package assets

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func requireMathPrograms(t testing.TB) {
	t.Helper()
	for _, name := range []string{"node", "rsvg-convert"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("optional %s unavailable: %v", name, err)
		}
	}
}

func TestMathCacheConcurrentInstallAndReuse(t *testing.T) {
	fakeMathInstall(t)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			root, err := InstallMath(context.Background())
			if err != nil || !strings.Contains(root, mathjaxVersion+"-"+mathPackageKey[:12]) {
				t.Errorf("root = %q, error = %v", root, err)
			}
		}()
	}
	wg.Wait()
	calls, err := os.ReadFile(os.Getenv("TTC_MATH_TEST_NPM_CALLS"))
	if err != nil || string(calls) != "npm\n" {
		t.Fatal("concurrent setup invoked npm more than once", string(calls), err)
	}
	root, err := mathRoot()
	if err != nil {
		t.Fatal(err)
	}
	if !readyMath(context.Background(), root) {
		t.Fatal("no validated package published")
	}
	st, err := os.Stat(root)
	if err != nil || st.Mode().Perm() != 0700 {
		t.Fatal("dependency cache must be private", st, err)
	}
	entries, err := os.ReadDir(filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".install-") {
			t.Fatal("staging directory leaked", entry.Name())
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := InstallMath(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled setup did not preserve cancellation", err)
	}
	if !readyMath(context.Background(), root) {
		t.Fatal("canceled setup damaged existing package")
	}
}

func TestMathJaxFormulasAndErrors(t *testing.T) {
	isolatedMathCache(t)
	r, err := NewMathRenderer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, tex := range []string{
		`e^{i\pi}+1=0`, `\frac{\partial u}{\partial t}=\alpha\nabla^2 u`,
		`\begin{pmatrix}1&2\\3&4\end{pmatrix}`,
		`\begin{aligned}a&=b+c\\x&=\frac{-b\pm\sqrt{b^2-4ac}}{2a}\end{aligned}`,
		`\mathbb{R}\times\mathfrak{g}+\mathcal{F}+\alpha\longrightarrow\infty`,
		`\hat{x}+\bar{y}+\vec{z}+\left\langle\sum_{k=0}^n k\right\rangle`,
		`\mathsf{ABC}+\mathtt{123}+\mathbf{A}+\text{μ Δ 漢字}`,
	} {
		m, err := r.Render(context.Background(), tex, 16)
		if err != nil || m == nil || m.Bounds().Dx() < 1 || m.Bounds().Dy() < 1 {
			t.Fatalf("%q: %v", tex, err)
		}
	}
	for _, tex := range []string{
		" ", `\ttcunknowncommand`, `\require{html}`, `\href{https://example.com}{x}`,
		`\def\a{\a}\a`, `\rule{100000em}{100000em}`,
	} {
		if _, err := r.Render(context.Background(), tex, 16); err == nil {
			t.Errorf("accepted unsupported or unbounded TeX %q", tex)
		}
	}
	root, _ := mathRoot()
	path := filepath.Join(root, "node_modules", "@mathjax", "src", "package.json")
	if err := os.WriteFile(path, []byte(`{"name":"@mathjax/src","version":"0.0.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkMath(context.Background(), root); err == nil || !strings.Contains(err.Error(), "require @mathjax/src@") {
		t.Fatal("accepted incompatible metadata", err)
	}
}

func TestDamagedMathCacheRebuildIsStagedAndUnsafePathsRejected(t *testing.T) {
	fakeMathInstall(t)
	root, err := ensureMath(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(root, "node_modules", "@mathjax", "src", "package.json")
	broken := []byte(`{"name":"@mathjax/src","version":"broken"}`)
	if err = os.WriteFile(metadata, broken, 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err = os.WriteFile(filepath.Join(bin, "node"), []byte("#!/bin/sh\nprintf 'fixture renderer failed' >&2\nexit 17\n"), 0700); err != nil {
		t.Fatal(err)
	}
	originalPath := os.Getenv("PATH")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+originalPath)
	if _, err = ensureMath(context.Background()); err == nil || !strings.Contains(err.Error(), "fixture renderer failed") {
		t.Fatal("failed validation was hidden", err)
	}
	retained, err := os.ReadFile(metadata)
	if err != nil || !bytes.Equal(retained, broken) {
		t.Fatal("failed setup modified existing cache", err)
	}
	t.Setenv("PATH", originalPath)
	if _, err = ensureMath(context.Background()); err != nil || !readyMath(context.Background(), root) {
		t.Fatal("bounded cache rebuild failed", err)
	}
	if err = os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err = os.Symlink(other, root); err != nil {
		t.Fatal(err)
	}
	if _, err = ensureMath(context.Background()); err == nil || !strings.Contains(err.Error(), "unsafe MathJax cache") {
		t.Fatal("accepted unsafe cache symlink", err)
	}
	if st, err := os.Lstat(root); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("changed unsafe existing path", err)
	}
}

func TestRenderProcessCancellationAndCaptureLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := renderOutput(ctx, exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30"), nil, 128)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("renderer ignored cancellation", err)
	}
	ctx = context.Background()
	_, err = renderOutput(ctx, exec.CommandContext(ctx, "/bin/sh", "-c", "printf '%4096s' x"), nil, 128)
	if err == nil || !strings.Contains(err.Error(), "output limit") {
		t.Fatal("renderer ignored capture limit", err)
	}
}

func BenchmarkMathJax(b *testing.B) {
	isolatedMathCache(b)
	start := time.Now()
	renderer, err := NewMathRenderer(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	defer renderer.Close()
	b.Logf("first-use pre-warmed process startup: %s", time.Since(start))
	for name, tex := range map[string]string{
		"inline":  `e^{i\pi}+1=0`,
		"matrix":  `\begin{pmatrix}1&2\\3&4\end{pmatrix}`,
		"aligned": `\begin{aligned}a&=b+c\\x&=\frac{-b\pm\sqrt{b^2-4ac}}{2a}\end{aligned}`,
	} {
		b.Run(name, func(b *testing.B) {
			for b.Loop() {
				if _, err := renderer.Render(context.Background(), tex, 16); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestMathCacheLocationAndMissingHostPrograms(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/cache-fixture")
	root, err := mathRoot()
	if err != nil || !strings.HasPrefix(root, "/cache-fixture/ttc/mathjax/"+mathjaxVersion) {
		t.Fatal(root, err)
	}
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "/home-fixture")
	root, err = mathRoot()
	if err != nil || !strings.HasPrefix(root, "/home-fixture/.cache/ttc/mathjax/") {
		t.Fatal(root, err)
	}
	t.Setenv("XDG_CACHE_HOME", "relative")
	if _, err = mathRoot(); err == nil {
		t.Fatal("accepted relative cache")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	if _, err = InstallMath(context.Background()); err == nil || !strings.Contains(err.Error(), "node is required") {
		t.Fatal("missing host program not disclosed", err)
	}
}

func TestMathInstallLockCancellation(t *testing.T) {
	fakeMathInstall(t)
	root, _ := mathRoot()
	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(parent, "install.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err = ensureMath(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("install lock ignored cancellation", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("canceled install published a package", err)
	}
}
