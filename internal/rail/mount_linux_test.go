package rail

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type mountFixture struct {
	root string
	opts SandboxOptions
	env  map[string]string
}

func newMountFixture(t *testing.T) *mountFixture {
	t.Helper()
	f := &mountFixture{root: t.TempDir(), env: map[string]string{}, opts: SandboxOptions{
		Workdir: "/home/user/projects/demo", Home: "/home/user", DataDir: "/home/user/.local/share/ttc",
		CacheDir: "/home/user/.cache/ttc", ConfigHome: "/home/user/.config", Executable: "/opt/ttc",
		SocketDir: "/registry/instance/private", Hostname: "host",
	}}
	for _, dir := range []string{"/usr/bin", "/usr/lib", "/etc", "/dev", "/run", f.opts.Workdir, f.opts.Home, f.opts.DataDir, f.opts.CacheDir, f.opts.SocketDir} {
		f.dir(t, dir)
	}
	f.file(t, "/opt/ttc")
	f.file(t, "/dev/null")
	for path, target := range map[string]string{"/bin": "usr/bin", "/sbin": "usr/bin", "/lib": "usr/lib", "/lib64": "usr/lib", "/var/run": "../run"} {
		f.link(t, path, target)
	}
	return f
}

func (f *mountFixture) host(path string) string { return filepath.Join(f.root, path) }
func (f *mountFixture) dir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(f.host(path), 0700); err != nil {
		t.Fatal(err)
	}
}
func (f *mountFixture) file(t *testing.T, path string) {
	t.Helper()
	f.dir(t, filepath.Dir(path))
	if err := os.WriteFile(f.host(path), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
}
func (f *mountFixture) link(t *testing.T, path, target string) {
	t.Helper()
	f.dir(t, filepath.Dir(path))
	if err := os.Symlink(target, f.host(path)); err != nil {
		t.Fatal(err)
	}
}
func (f *mountFixture) args(t *testing.T) []string {
	t.Helper()
	args, err := sandboxArgs(f.opts, f.root, func(key string) string { return f.env[key] })
	if err != nil {
		t.Fatal(err)
	}
	return args
}
func mountArgIndex(args []string, wanted ...string) int {
	for i := 0; i+len(wanted) <= len(args); i++ {
		if reflect.DeepEqual(args[i:i+len(wanted)], wanted) {
			return i
		}
	}
	return -1
}
func requireMountArgs(t *testing.T, args []string, wanted ...string) int {
	t.Helper()
	index := mountArgIndex(args, wanted...)
	if index < 0 {
		t.Fatalf("missing arguments %q in %q", wanted, args)
	}
	return index
}

func TestSandboxMinimalRootAndLifecycle(t *testing.T) {
	f := newMountFixture(t)
	args := f.args(t)
	for _, option := range []string{"--unshare-user", "--unshare-pid", "--unshare-uts", "--new-session"} {
		requireMountArgs(t, args, option)
	}
	requireMountArgs(t, args, "--hostname", "host-ttc")
	requireMountArgs(t, args, "--cap-drop", "ALL")
	for _, option := range []string{"--unshare-net", "--die-with-parent", "--as-pid-1", "--"} {
		if mountArgIndex(args, option) >= 0 {
			t.Errorf("unexpected %s", option)
		}
	}
	for i, arg := range args {
		if (arg == "--bind" || arg == "--ro-bind") && args[i+1] == f.root {
			t.Fatal("host root exposed")
		}
	}
	requireMountArgs(t, args, "--ro-bind", f.host("/usr"), "/usr")
	requireMountArgs(t, args, "--ro-bind", f.host("/etc"), "/etc")
	requireMountArgs(t, args, "--symlink", "usr/bin", "/bin")
	requireMountArgs(t, args, "--symlink", "usr/lib", "/lib64")
	requireMountArgs(t, args, "--proc", "/proc")
	requireMountArgs(t, args, "--dev", "/dev")
	for _, path := range []string{"/tmp", "/run", f.opts.Home} {
		requireMountArgs(t, args, "--tmpfs", path)
	}
	for _, path := range []string{f.opts.Workdir, f.opts.DataDir, f.opts.CacheDir} {
		requireMountArgs(t, args, "--bind", f.host(path), path)
	}
	requireMountArgs(t, args, "--ro-bind", f.host(f.opts.Executable), f.opts.Executable)
	requireMountArgs(t, args, "--bind", f.host(f.opts.SocketDir), "/run/ttc-rail")
	if strings.Contains(strings.Join(args, "\n"), f.host("/registry/instance")+"\n") {
		t.Fatal("host supervisor metadata directory exposed")
	}
	requireMountArgs(t, args, "--chdir", f.opts.Workdir)
}

func TestSandboxNonMergedSystemDirectories(t *testing.T) {
	f := newMountFixture(t)
	for _, path := range []string{"/bin", "/sbin", "/lib", "/lib64"} {
		if err := os.Remove(f.host(path)); err != nil {
			t.Fatal(err)
		}
		f.dir(t, path)
	}
	args := f.args(t)
	for _, path := range []string{"/bin", "/sbin", "/lib", "/lib64"} {
		requireMountArgs(t, args, "--ro-bind", f.host(path), path)
	}
}

func TestSandboxResolverSymlinkDoesNotExposeHostRun(t *testing.T) {
	f := newMountFixture(t)
	f.file(t, "/run/systemd/resolve/resolv.conf")
	f.link(t, "/etc/resolv.conf", "/run/systemd/resolve/resolv.conf")
	args := f.args(t)
	requireMountArgs(t, args, "--ro-bind", f.host("/run/systemd/resolve/resolv.conf"), "/run/systemd/resolve/resolv.conf")
	if mountArgIndex(args, "--ro-bind", f.host("/run"), "/run") >= 0 {
		t.Fatal("resolver exposed host runtime directory")
	}
}

func TestSandboxShellAndDiscoveryDefaults(t *testing.T) {
	f := newMountFixture(t)
	f.opts.ConfigHome = "/settings"
	f.env["ZDOTDIR"] = "/settings/zsh-custom"
	files := []string{".bashrc", ".bash_profile", ".bash_login", ".profile", ".zshenv", ".zprofile", ".zshrc", ".zlogin", ".zlogout", ".tmux.conf"}
	var paths []string
	for _, name := range files {
		path := filepath.Join(f.opts.Home, name)
		f.file(t, path)
		paths = append(paths, path)
	}
	for _, path := range []string{"/home/user/.config/zsh", "/home/user/.config/tmux", "/settings/zsh", "/settings/tmux", "/settings/ttc", "/settings/zsh-custom", "/home/user/.agents/skill", "/home/user/.agents/skills", "/home/user/projects/.agents/skills", "/.agents/skills"} {
		f.dir(t, path)
		paths = append(paths, path)
	}
	for _, path := range []string{"/AGENTS.md", "/home/AGENTS.md", "/home/user/projects/AGENTS.md"} {
		f.file(t, path)
		paths = append(paths, path)
	}
	args := f.args(t)
	for _, path := range paths {
		requireMountArgs(t, args, "--ro-bind", f.host(path), path)
	}
	if mountArgIndex(args, "--bind", f.host(f.opts.Home), f.opts.Home) >= 0 {
		t.Fatal("host home exposed")
	}
}

func TestSandboxDefaultMissingAndExplicitMissing(t *testing.T) {
	f := newMountFixture(t)
	f.args(t) // No shell, skills or global configuration files are required.
	f.opts.Policy.Mounts = []Mount{{Source: "/missing", Dest: "/extra"}}
	if _, err := sandboxArgs(f.opts, f.root, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "/extra") {
		t.Fatalf("explicit missing source error: %v", err)
	}
}

func TestSandboxMountOrderAndExactOverride(t *testing.T) {
	f := newMountFixture(t)
	f.file(t, "/home/user/.bashrc")
	f.file(t, "/other/bashrc")
	f.dir(t, "/other/parent")
	f.dir(t, "/other/child")
	f.opts.Policy.Mounts = []Mount{
		{Source: "/other/child", Dest: "/extra/child", Writable: true},
		{Source: "/other/parent", Dest: "/extra"},
		{Source: "/other/bashrc", Dest: "/home/user/.bashrc", Writable: true},
	}
	args := f.args(t)
	parent := requireMountArgs(t, args, "--ro-bind", f.host("/other/parent"), "/extra")
	child := requireMountArgs(t, args, "--bind", f.host("/other/child"), "/extra/child")
	if parent > child {
		t.Fatal("descendant mounted before ancestor")
	}
	requireMountArgs(t, args, "--bind", f.host("/other/bashrc"), "/home/user/.bashrc")
	if mountArgIndex(args, "--ro-bind", f.host("/home/user/.bashrc"), "/home/user/.bashrc") >= 0 {
		t.Fatal("default was not replaced by exact destination override")
	}
}

func TestSandboxMountOrderAccountsForDestinationSymlinks(t *testing.T) {
	f := newMountFixture(t)
	f.file(t, "/other/tool")
	f.dir(t, "/other/parent")
	f.dir(t, "/other/target")
	f.link(t, "/other/parent/link", "/target/nested")
	f.opts.Policy.Mounts = []Mount{
		{Source: "/other/tool", Dest: "/bin/tool", Writable: true},
		{Source: "/other/parent", Dest: "/extra"},
		{Source: "/other/tool", Dest: "/extra/link", Writable: true},
		{Source: "/other/target", Dest: "/target"},
	}
	args := f.args(t)
	usr := requireMountArgs(t, args, "--ro-bind", f.host("/usr"), "/usr")
	tool := requireMountArgs(t, args, "--bind", f.host("/other/tool"), "/usr/bin/tool")
	target := requireMountArgs(t, args, "--ro-bind", f.host("/other/target"), "/target")
	redirected := requireMountArgs(t, args, "--bind", f.host("/other/tool"), "/target/nested")
	if usr >= tool || target >= redirected {
		t.Fatal("a canonical ancestor hid the descendant overlay")
	}
}

func TestSandboxRejectsAmbiguousCanonicalDestinations(t *testing.T) {
	f := newMountFixture(t)
	f.file(t, "/other/tool")
	f.opts.Policy.Mounts = []Mount{{Source: "/other/tool", Dest: "/bin/tool"}, {Source: "/other/tool", Dest: "/usr/bin/tool"}}
	if _, err := sandboxArgs(f.opts, f.root, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "same path") {
		t.Fatalf("ambiguous alias destinations: %v", err)
	}
}

func TestSandboxConfigsReadonlyAfterWritableMountsAndSymlinkAliases(t *testing.T) {
	f := newMountFixture(t)
	config := filepath.Join(f.opts.Workdir, "settings.json")
	link := filepath.Join(f.opts.Workdir, "ttc-rail.json")
	f.file(t, config)
	f.link(t, link, "settings.json")
	f.opts.Policy.ConfigFiles = []string{link}
	f.opts.Policy.Mounts = []Mount{{Source: f.opts.Workdir, Dest: "/alias", Writable: true}, {Source: config, Dest: "/direct", Writable: true}}
	args := f.args(t)
	writable := requireMountArgs(t, args, "--bind", f.host(f.opts.Workdir), f.opts.Workdir)
	readonly := requireMountArgs(t, args, "--ro-bind", f.host(config), config)
	if readonly <= writable {
		t.Fatal("mandatory config overlay precedes writable workspace")
	}
	requireMountArgs(t, args, "--ro-bind", f.host(config), "/alias/settings.json")
	requireMountArgs(t, args, "--ro-bind", f.host(config), "/direct")
}

func TestSandboxAbsoluteSourceSymlinksAreFixtureRooted(t *testing.T) {
	f := newMountFixture(t)
	f.file(t, "/secrets/bashrc")
	f.link(t, "/home/user/.bashrc", "/secrets/bashrc")
	f.opts.Policy.Mounts = []Mount{{Source: "/secrets", Dest: "/allowed", Writable: true}}
	args := f.args(t)
	requireMountArgs(t, args, "--ro-bind", f.host("/secrets/bashrc"), "/home/user/.bashrc")
	requireMountArgs(t, args, "--ro-bind", f.host("/secrets/bashrc"), "/allowed/bashrc")
}

func TestSandboxDenyWinsAfterAllMounts(t *testing.T) {
	f := newMountFixture(t)
	f.dir(t, "/outside")
	f.dir(t, "/outside/child")
	file := filepath.Join(f.opts.Workdir, "ttc-rail.json")
	f.file(t, file)
	f.opts.Policy.ConfigFiles = []string{file}
	f.opts.Policy.Mounts = []Mount{{Source: "/outside", Dest: "/extra", Writable: true}, {Source: "/outside/child", Dest: "/extra/child", Writable: true}}
	f.opts.Policy.Denies = []string{"/extra/child", file, "/extra"}
	args := f.args(t)
	denyDir := requireMountArgs(t, args, "--tmpfs", "/extra", "--remount-ro", "/extra")
	denyFile := requireMountArgs(t, args, "--ro-bind", f.host("/dev/null"), file)
	for _, mount := range [][]string{{"--bind", f.host("/outside/child"), "/extra/child"}, {"--ro-bind", f.host(file), file}} {
		if requireMountArgs(t, args, mount...) >= denyDir || requireMountArgs(t, args, mount...) >= denyFile {
			t.Fatal("deny masks must follow all mounts")
		}
	}
	if mountArgIndex(args, "--tmpfs", "/extra/child") >= 0 {
		t.Fatal("redundant nested deny can reopen denied ancestor")
	}
}

func TestSandboxDenySymlinkMasksItsVisibleTarget(t *testing.T) {
	f := newMountFixture(t)
	f.file(t, filepath.Join(f.opts.Workdir, "secret"))
	f.link(t, filepath.Join(f.opts.Workdir, "link"), "secret")
	f.opts.Policy.Denies = []string{filepath.Join(f.opts.Workdir, "link")}
	requireMountArgs(t, f.args(t), "--ro-bind", f.host("/dev/null"), filepath.Join(f.opts.Workdir, "secret"))
}

func TestSandboxRejectsWorkspaceAndReservedDenies(t *testing.T) {
	for _, path := range []string{"/home", "/home/user/projects", "/home/user/projects/demo", "/proc", "/dev/null", "/run", "/run/ttc-rail/config"} {
		t.Run(path, func(t *testing.T) {
			f := newMountFixture(t)
			f.opts.Policy.Denies = []string{path}
			if _, err := sandboxArgs(f.opts, f.root, func(string) string { return "" }); err == nil {
				t.Fatal("accepted conflicting deny")
			}
		})
	}
}

func TestSandboxRejectsReservedMountsIncludingSymlinkRedirect(t *testing.T) {
	for _, dest := range []string{"/", "/proc", "/proc/sys", "/dev", "/run", "/run/ttc-rail", "/run/ttc-rail/config"} {
		t.Run(dest, func(t *testing.T) {
			f := newMountFixture(t)
			f.opts.Policy.Mounts = []Mount{{Source: "/usr", Dest: dest}}
			if _, err := sandboxArgs(f.opts, f.root, func(string) string { return "" }); err == nil {
				t.Fatal("accepted reserved mount")
			}
		})
	}
	f := newMountFixture(t)
	f.link(t, filepath.Join(f.opts.Workdir, "redirect"), "/run/ttc-rail")
	f.opts.Policy.Mounts = []Mount{{Source: "/usr", Dest: filepath.Join(f.opts.Workdir, "redirect")}}
	if _, err := sandboxArgs(f.opts, f.root, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("symlink redirected mount should fail: %v", err)
	}
}

func TestSandboxDockerSystemSocketAndDeny(t *testing.T) {
	f := newMountFixture(t)
	// Listen creates a real Unix socket; it is entirely local to this fixture.
	listener, err := net.Listen("unix", f.host("/run/docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	f.opts.Policy.Docker = true
	f.opts.Policy.Denies = []string{"/var/run/docker.sock"}
	args := f.args(t)
	bind := requireMountArgs(t, args, "--bind", f.host("/run/docker.sock"), "/run/docker.sock")
	deny := requireMountArgs(t, args, "--ro-bind", f.host("/dev/null"), "/run/docker.sock")
	if deny <= bind {
		t.Fatal("Docker authorization overrode deny")
	}
	f.env["DOCKER_HOST"] = "tcp://127.0.0.1:2375"
	if _, err := sandboxArgs(f.opts, f.root, func(key string) string { return f.env[key] }); err == nil || !strings.Contains(err.Error(), "DOCKER_HOST") {
		t.Fatalf("unsupported Docker host: %v", err)
	}
}

func TestDockerAccessGate(t *testing.T) {
	for _, source := range []string{"/var/run/docker.sock", "/run/docker.sock", "/run", "/socket-alias"} {
		for _, writable := range []bool{false, true} {
			f := newMountFixture(t)
			listener, err := net.Listen("unix", f.host("/run/docker.sock"))
			if err != nil {
				t.Fatal(err)
			}
			f.link(t, "/socket-alias", "/run/docker.sock")
			f.opts.Policy.Mounts = []Mount{{Source: source, Dest: "/extra", Writable: writable}}
			if _, err := sandboxArgs(f.opts, f.root, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "Docker") {
				t.Fatalf("unauthorized Docker mount accepted: %s rw=%v: %v", source, writable, err)
			}
			f.opts.Policy.Docker = true
			f.args(t)
			listener.Close()
		}
	}
}

func TestSandboxDockerRejectsMissingOrNonSocket(t *testing.T) {
	for _, create := range []bool{false, true} {
		f := newMountFixture(t)
		if create {
			f.file(t, "/run/docker.sock")
		}
		f.opts.Policy.Docker = true
		if _, err := sandboxArgs(f.opts, f.root, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "socket") {
			t.Fatalf("missing/non-socket Docker source: %v", err)
		}
	}
}

func TestSandboxRequiredPathsAndExecutableInsideWorkdir(t *testing.T) {
	f := newMountFixture(t)
	f.opts.Executable = filepath.Join(f.opts.Workdir, "ttc")
	f.file(t, f.opts.Executable)
	args := f.args(t)
	requireMountArgs(t, args, "--ro-bind", f.host(f.opts.Executable), f.opts.Executable)
	if err := os.RemoveAll(f.host(f.opts.DataDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := sandboxArgs(f.opts, f.root, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "writable directory") {
		t.Fatalf("missing required writable directory: %v", err)
	}
}

func TestSandboxGlobalConfigSymlinkTargetIsReadonly(t *testing.T) {
	f := newMountFixture(t)
	target := filepath.Join(f.opts.Workdir, "search-settings.json")
	f.file(t, target)
	f.link(t, filepath.Join(f.opts.ConfigHome, "ttc", "web-search.json"), target)
	args := f.args(t)
	readonly := requireMountArgs(t, args, "--ro-bind", f.host(target), target)
	writable := requireMountArgs(t, args, "--bind", f.host(f.opts.Workdir), f.opts.Workdir)
	// The initial entrypoint overlay can precede the writable workspace, but
	// alias protection must overlay it again after all writable mounts.
	if mountArgIndex(args[writable+3:], "--ro-bind", f.host(target), target) < 0 {
		t.Fatalf("configuration source was writable after workspace overlay (first RO mount %d)", readonly)
	}
}

func TestSandboxZdotdirHomeDoesNotExposeHostHome(t *testing.T) {
	f := newMountFixture(t)
	f.env["ZDOTDIR"] = f.opts.Home
	f.file(t, filepath.Join(f.opts.Home, ".zshrc"))
	args := f.args(t)
	if mountArgIndex(args, "--ro-bind", f.host(f.opts.Home), f.opts.Home) >= 0 {
		t.Fatal("ZDOTDIR=$HOME exposed the entire host home")
	}
	requireMountArgs(t, args, "--ro-bind", f.host(filepath.Join(f.opts.Home, ".zshrc")), filepath.Join(f.opts.Home, ".zshrc"))
	for _, value := range []string{"relative", "/", f.opts.Workdir} {
		f.env["ZDOTDIR"] = value
		if _, err := sandboxArgs(f.opts, f.root, func(key string) string { return f.env[key] }); err == nil {
			t.Fatalf("accepted unsafe ZDOTDIR %q", value)
		}
	}
}

func TestSandboxMandatoryConfigAndSystemErrors(t *testing.T) {
	for _, kind := range []string{"missing config", "directory config", "missing usr", "symlink loop", "relative workdir", "empty hostname", "workspace contains home"} {
		t.Run(kind, func(t *testing.T) {
			f := newMountFixture(t)
			switch kind {
			case "missing config":
				f.opts.Policy.ConfigFiles = []string{"/missing.json"}
			case "directory config":
				f.opts.Policy.ConfigFiles = []string{f.opts.Workdir}
			case "missing usr":
				if err := os.RemoveAll(f.host("/usr")); err != nil {
					t.Fatal(err)
				}
			case "symlink loop":
				f.link(t, "/cycle-a", "/cycle-b")
				f.link(t, "/cycle-b", "/cycle-a")
				f.opts.Policy.Mounts = []Mount{{Source: "/cycle-a", Dest: "/extra"}}
			case "relative workdir":
				f.opts.Workdir = "project"
			case "empty hostname":
				f.opts.Hostname = ""
			case "workspace contains home":
				f.opts.Workdir = f.opts.Home
			}
			if _, err := sandboxArgs(f.opts, f.root, func(string) string { return "" }); err == nil {
				t.Fatal("accepted invalid sandbox options")
			}
		})
	}
}
