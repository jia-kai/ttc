// Package rail launches persistent, workdir-scoped filesystem sandboxes.
package rail

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"ttc/internal/filelock"
	"ttc/internal/history"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const supervisorCommand = "__rail_serve"
const maxInstanceBytes = 64 << 10

// Options describes an attach/create or list operation. Input is the attached
// terminal; Output receives lists. Both belong to the caller. DataDir is TTC's
// existing shared data root, not a container registry.
type Options struct {
	Workdir, DataDir string
	List             bool
	Input            *os.File
	Output           io.Writer
}

type instance struct {
	Version   int    `json:"version"`
	Workdir   string `json:"workdir"`
	CreatedMS int64  `json:"created_ms"`
}

type launchSpec struct {
	Workdir, Home, ConfigHome, DataDir, CacheDir, Executable, Hostname string
}

// IsSupervisor identifies the private child entrypoint before CLI flag parsing.
func IsSupervisor(args []string) bool {
	return len(args) == 1 && args[0] == supervisorCommand
}

// Run lists live sessions or attaches to the single sandbox for the canonical
// workdir, creating it when absent. Detaching does not stop the sandbox.
func Run(ctx context.Context, opts Options) error {
	if os.Geteuid() == 0 {
		return errors.New("rail must run as your normal user, without sudo")
	}
	for _, tool := range []string{"tmux", "bwrap"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("rail requires %s (Arch: pacman -S bubblewrap tmux): %w", tool, err)
		}
	}
	root, err := runtimeRoot()
	if err != nil {
		return err
	}
	if opts.List {
		return listInstances(ctx, root, opts.Output)
	}
	if opts.Input == nil || !term.IsTerminal(int(opts.Input.Fd())) {
		return errors.New("rail attach requires a terminal; use --list for noninteractive inspection")
	}
	workdir, err := canonicalWorkdir(opts.Workdir)
	if err != nil {
		return err
	}
	dir := filepath.Join(root, workdirKey(workdir))
	if err = privateDir(dir); err != nil {
		return err
	}
	lock, err := filelock.Acquire(ctx, filepath.Join(dir, "create.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	live, err := serverRunning(dir)
	if err != nil {
		return err
	}
	if live {
		info, err := readInstance(dir)
		if err != nil {
			return err
		}
		if info.Workdir != workdir {
			return errors.New("rail registry workdir identity mismatch")
		}
		if _, err = sessions(ctx, controlPath(dir)); err != nil {
			return fmt.Errorf("rail server is running but unavailable; inspect %s: %w", filepath.Join(dir, "server.log"), err)
		}
	} else if err = launch(ctx, dir, workdir, opts.DataDir); err != nil {
		return err
	}
	// Other clients can attach while this one remains attached.
	if err = lock.Close(); err != nil {
		return err
	}
	return attachClient(ctx, controlPath(dir), opts.Input)
}

func canonicalWorkdir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve rail workdir: %w", err)
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("rail workdir must be an existing directory: %s", abs)
	}
	return abs, nil
}

func workdirKey(path string) string {
	hash := sha256.Sum256([]byte(path))
	return hex.EncodeToString(hash[:12])
}

func runtimeRoot() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	var root string
	if base != "" {
		if !filepath.IsAbs(base) {
			return "", errors.New("XDG_RUNTIME_DIR must be absolute")
		}
		if err := checkPrivateDir(base); err != nil {
			return "", fmt.Errorf("XDG_RUNTIME_DIR: %w", err)
		}
		parent := filepath.Join(base, "ttc")
		if err := privateDir(parent); err != nil {
			return "", err
		}
		root = filepath.Join(parent, "rail")
	} else {
		root = filepath.Join(os.TempDir(), fmt.Sprintf("ttc-rail-%d", os.Getuid()))
	}
	if err := privateDir(root); err != nil {
		return "", err
	}
	// sockaddr_un has a small fixed-size path field on Linux.
	if len(socketPath(filepath.Join(root, strings.Repeat("0", 24)))) >= 108 {
		return "", errors.New("rail runtime path is too long for a Unix socket; use a shorter XDG_RUNTIME_DIR")
	}
	return root, nil
}

func privateDir(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return checkPrivateDir(path)
}

func checkPrivateDir(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.IsDir() || st.Mode().Perm() != 0700 || stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%s must be an owned, nonsymlink 0700 directory", path)
	}
	return nil
}

func socketPath(dir string) string { return filepath.Join(dir, "run", "server.sock") }

func controlPath(dir string) string { return filepath.Join(dir, "run", "control.sock") }

// The lifetime flock avoids PID reuse and does not survive supervisor death.
func serverRunning(dir string) (bool, error) {
	f, err := os.OpenFile(filepath.Join(dir, "server.lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		return false, errors.New("rail server lock must be a private regular file")
	}
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func readInstance(dir string) (instance, error) {
	var info instance
	f, err := os.OpenFile(filepath.Join(dir, "instance.json"), os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return info, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return info, err
	}
	if !stat.Mode().IsRegular() {
		return info, errors.New("rail instance metadata must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxInstanceBytes+1))
	if err != nil {
		return info, err
	}
	if len(data) > maxInstanceBytes {
		return info, errors.New("rail instance metadata is too large")
	}
	if err = json.Unmarshal(data, &info); err != nil {
		return info, err
	}
	if info.Version != 1 || !filepath.IsAbs(info.Workdir) {
		return info, errors.New("incompatible or invalid rail instance metadata")
	}
	return info, nil
}

func launch(ctx context.Context, dir, workdir, dataDir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	config, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	spec := launchSpec{workdir, home, config, dataDir, filepath.Join(cache, "ttc"), executable, host}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	ready, childReady, err := os.Pipe()
	if err != nil {
		return err
	}
	defer ready.Close()
	defer childReady.Close()
	log, err := os.OpenFile(filepath.Join(dir, "server.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(executable, supervisorCommand)
	cmd.Env = clientEnvironment()
	cmd.Stdin = bytes.NewReader(encoded)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.ExtraFiles = []*os.File{childReady}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return err
	}
	childReady.Close()
	// Reap the launcher child without tying the detached server to this client.
	go func() { _ = cmd.Wait() }()
	result := make(chan []byte, 1)
	go func() {
		line, _ := bufio.NewReader(io.LimitReader(ready, 64<<10)).ReadString('\n')
		result <- []byte(line)
	}()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case data := <-result:
		if string(data) == "ready\n" {
			return nil
		}
		return fmt.Errorf("rail startup failed: %s (log: %s)", strings.TrimSpace(string(data)), filepath.Join(dir, "server.log"))
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return ctx.Err()
	case <-timer.C:
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return fmt.Errorf("rail startup timed out; inspect %s", filepath.Join(dir, "server.log"))
	}
}

// Serve is the detached supervisor entrypoint. It reads a private launch spec
// from stdin and acknowledges readiness on inherited FD 3. Its lifetime lock and
// Bubblewrap's parent-death handling keep the registry and sandbox consistent.
func Serve(ctx context.Context) (err error) {
	ready := os.NewFile(3, "rail-ready")
	unix.CloseOnExec(3) // Never expose the launcher handshake to sandbox processes.
	defer ready.Close()
	defer func() {
		if err != nil {
			_, _ = fmt.Fprintln(ready, err)
		}
	}()
	var spec launchSpec
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 64<<10))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&spec); err != nil {
		return err
	}
	root, err := runtimeRoot()
	if err != nil {
		return err
	}
	dir := filepath.Join(root, workdirKey(spec.Workdir))
	if err = checkPrivateDir(dir); err != nil {
		return err
	}
	lock, err := filelock.Acquire(ctx, filepath.Join(dir, "server.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	runDir := filepath.Join(dir, "run")
	if err = os.RemoveAll(runDir); err != nil {
		return err
	}
	if err = privateDir(runDir); err != nil {
		return err
	}
	defer os.RemoveAll(runDir)
	defer os.Remove(filepath.Join(dir, "instance.json"))
	policy, err := LoadPolicy(spec.Home, spec.ConfigHome, spec.Workdir)
	if err != nil {
		return err
	}
	for _, path := range []string{spec.DataDir, spec.CacheDir} {
		if err = history.PrivateDir(path); err != nil {
			return err
		}
	}
	environment := clientEnvironment()
	sandbox, err := resolveSandboxSpec(ctx, sandboxOptions{
		Workdir: spec.Workdir, Home: spec.Home, DataDir: spec.DataDir,
		CacheDir: spec.CacheDir, ConfigHome: spec.ConfigHome, Executable: spec.Executable,
		SocketDir: runDir, Hostname: spec.Hostname, Policy: policy,
	}, planningEnvironment(environment, os.Geteuid()))
	if err != nil {
		return err
	}
	plan, err := planSandbox(ctx, sandbox, "/")
	if err != nil {
		return err
	}
	command, err := bubblewrapInvocation(*plan, sandboxProcess{
		command:     []string{spec.Executable, clientServerCommand},
		environment: environment, dieWithParent: true,
	})
	if err != nil {
		return err
	}
	startup := tmuxStartup(*plan)
	if err = os.WriteFile(filepath.Join(runDir, "tmux.conf"), []byte(startup), 0600); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, command.executable, command.args...)
	cmd.Env = command.environment
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err = plan.validateHost(ctx); err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	startupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var probeErr error
	for {
		select {
		case waitErr := <-exited:
			return fmt.Errorf("sandbox exited before tmux was ready: %w", waitErr)
		default:
		}
		if _, probeErr = sessions(startupCtx, controlPath(dir)); probeErr == nil {
			break
		}
		select {
		case <-startupCtx.Done():
			_ = cmd.Process.Kill()
			<-exited
			return fmt.Errorf("tmux did not become ready: %w (last session probe: %v)", startupCtx.Err(), probeErr)
		case <-time.After(50 * time.Millisecond):
		}
	}
	data, err := json.Marshal(instance{Version: 1, Workdir: spec.Workdir, CreatedMS: time.Now().UnixMilli()})
	if err != nil {
		_ = cmd.Process.Kill()
		<-exited
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "instance.json"), data, 0600); err != nil {
		_ = cmd.Process.Kill()
		<-exited
		return err
	}
	if _, err = io.WriteString(ready, "ready\n"); err != nil {
		_ = cmd.Process.Kill()
		<-exited
		return err
	}
	ready.Close()
	return <-exited
}

func tmuxQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func tmuxStartup(plan sandboxPlan) string {
	var b strings.Builder
	b.WriteString("source-file -q /etc/tmux.conf\n")
	if plan.tmuxConfig != "" {
		fmt.Fprintf(&b, "source-file %s\n", tmuxQuote(plan.tmuxConfig))
	}
	// Explicit lifetime settings override user config only for this server.
	b.WriteString("set-option -g exit-unattached off\n")
	fmt.Fprintf(&b, "new-session -d -s rail -c %s\n", tmuxQuote(plan.workdir))
	b.WriteString("set-option -g exit-empty on\n")
	fmt.Fprintf(&b, "set-option -g status-left %s\n", tmuxQuote("[#S] #H · "+filepath.Base(plan.workdir)+" "))
	return b.String()
}

func clientEnvironment() []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "TMUX=") && !strings.HasPrefix(entry, "TMUX_PANE=") {
			env = append(env, entry)
		}
	}
	return env
}

func listInstances(ctx context.Context, root string, output io.Writer) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	type row struct{ workdir, session string }
	var rows []row
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() || len(entry.Name()) != 24 {
			continue
		}
		if _, err := hex.DecodeString(entry.Name()); err != nil {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if err := checkPrivateDir(dir); err != nil {
			return err
		}
		live, err := serverRunning(dir)
		if err != nil {
			return err
		}
		if !live {
			continue
		}
		info, err := readInstance(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue // Starting or stopping, not yet a live session to report.
		}
		if err != nil {
			return err
		}
		if workdirKey(info.Workdir) != entry.Name() {
			return errors.New("rail registry workdir identity mismatch")
		}
		lines, err := sessions(ctx, controlPath(dir))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue // Server may have exited between the lock and socket checks.
		}
		for _, line := range lines {
			rows = append(rows, row{info.Workdir, line})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].workdir != rows[j].workdir {
			return rows[i].workdir < rows[j].workdir
		}
		return rows[i].session < rows[j].session
	})
	if len(rows) == 0 {
		_, err := fmt.Fprintln(output, "No live rail sessions.")
		return err
	}
	w := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "WORKDIR\tSESSION\tWINDOWS\tATTACHED")
	for _, row := range rows {
		fmt.Fprintf(w, "%s\t%s\n", row.workdir, row.session)
	}
	return w.Flush()
}
