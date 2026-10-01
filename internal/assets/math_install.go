package assets

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"scicode/internal/history"

	"golang.org/x/sys/unix"
)

//go:embed mathjax-package.json
var mathPackage string

//go:embed mathjax-package-lock.json
var mathLock string

var mathPackageKey = Key(mathPackage, mathLock)

// InstallMath installs the pinned MathJax packages in the user cache if needed.
// npm verifies lockfile SHA-512 integrity values; scripts are disabled. Setup is
// staged, validated and serialized, with bounded output and a two-minute deadline.
// It returns the published cache path; no npm invocation occurs for a ready cache.
func InstallMath(ctx context.Context) (string, error) { return ensureMath(ctx) }

// ensureMath publishes a validated immutable npm installation.
func ensureMath(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	for _, name := range []string{"node", "rsvg-convert"} {
		if _, err := exec.LookPath(name); err != nil {
			return "", fmt.Errorf("%s is required: %w", name, err)
		}
	}
	root, err := mathRoot()
	if err != nil {
		return "", err
	}
	if readyMath(ctx, root) {
		return root, nil
	}
	if st, err := os.Lstat(root); err == nil {
		if !st.IsDir() || st.Mode().Perm() != 0700 {
			return "", fmt.Errorf("unsafe MathJax cache directory %s: require private 0700 directory", root)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(root)
	if err = history.PrivateDir(parent); err != nil {
		return "", fmt.Errorf("prepare MathJax cache: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(parent, "install.lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	info, err := lock.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("MathJax install lock must be a regular file")
	}
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK {
			return "", fmt.Errorf("lock MathJax cache: %w", err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if readyMath(ctx, root) {
		return root, nil
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(parent, ".install-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if _, err = exec.LookPath("npm"); err != nil {
		return "", fmt.Errorf("npm is required to install MathJax: %w", err)
	}
	for name, data := range map[string]string{"package.json": mathPackage, "package-lock.json": mathLock} {
		if err = os.WriteFile(filepath.Join(stage, name), []byte(data), 0600); err != nil {
			return "", err
		}
	}
	downloadCache := filepath.Join(parent, "npm")
	if err = history.PrivateDir(downloadCache); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "npm", "ci", "--prefix", stage, "--cache", downloadCache,
		"--ignore-scripts", "--no-audit", "--no-fund", "--prefer-offline",
		"--global=false", "--workspaces=false", "--package-lock=true", "--install-strategy=hoisted",
		"--bin-links=false", "--fetch-retries=0", "--fetch-timeout=30000")
	cmd.Dir, cmd.Env = stage, mathEnvironment()
	if _, err = renderOutput(ctx, cmd, nil, 16<<10); err != nil {
		return "", fmt.Errorf("install pinned MathJax with npm ci: %w", err)
	}
	if err = checkMath(ctx, stage); err != nil {
		return "", err
	}
	if _, err = formulaAt(ctx, stage, `\frac{1}{2}`, 16); err != nil {
		return "", fmt.Errorf("validate installed MathJax: %w", err)
	}
	if err = os.WriteFile(filepath.Join(stage, ".ready"), []byte(mathPackageKey), 0600); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if _, err = os.Lstat(root); err == nil {
		if err = history.PrivateDir(root); err != nil {
			return "", err
		}
		if err = os.RemoveAll(root); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err = os.Rename(stage, root); err != nil {
		return "", fmt.Errorf("publish MathJax cache: %w", err)
	}
	return root, nil
}

func readyMath(ctx context.Context, root string) bool {
	st, err := os.Lstat(root)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0700 {
		return false
	}
	path := filepath.Join(root, ".ready")
	st, err = os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() != int64(len(mathPackageKey)) {
		return false
	}
	data, err := os.ReadFile(path)
	return err == nil && string(data) == mathPackageKey && checkMath(ctx, root) == nil
}
