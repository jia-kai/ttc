package rail

import (
	"context"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type mountFixture struct {
	root string
	opts sandboxOptions
	env  map[string]string
}

func newMountFixture(t *testing.T) *mountFixture {
	t.Helper()
	f := &mountFixture{root: t.TempDir(), env: map[string]string{}, opts: sandboxOptions{
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
func (f *mountFixture) environment() sandboxEnvironment {
	return sandboxEnvironment{
		SSHAuthSock: f.env["SSH_AUTH_SOCK"], DockerHost: f.env["DOCKER_HOST"],
		HistoryFile: f.env["HISTFILE"], ZDotDir: f.env["ZDOTDIR"],
		Path: "/usr/bin:/bin", UID: 1000,
	}
}
func (f *mountFixture) plan(ctx context.Context) (*sandboxPlan, error) {
	spec, err := resolveSandboxSpec(ctx, f.opts, f.environment())
	if err != nil {
		return nil, err
	}
	return planSandbox(ctx, spec, f.root)
}
func (f *mountFixture) mustPlan(t *testing.T) *sandboxPlan {
	t.Helper()
	plan, err := f.plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
func requireFilesystemOperation(t *testing.T, plan *sandboxPlan, wanted filesystemOperation) int {
	t.Helper()
	index := slices.Index(plan.filesystem, wanted)
	if index < 0 {
		t.Fatalf("missing filesystem operation %+v in %+v", wanted, plan.filesystem)
	}
	return index
}

func requireEnvironmentChange(t *testing.T, plan *sandboxPlan, wanted environmentChange) {
	t.Helper()
	if !slices.Contains(plan.environment, wanted) {
		t.Fatalf("missing environment change %+v in %+v", wanted, plan.environment)
	}
}

func TestSandboxMinimalRootAndLifecycle(t *testing.T) {
	f := newMountFixture(t)
	plan := f.mustPlan(t)
	if !slices.Equal(plan.namespaces, []namespaceKind{namespaceUser, namespaceProcess, namespaceHostname}) {
		t.Fatalf("unexpected namespaces: %v", plan.namespaces)
	}
	if plan.hostname != "host-ttc" || !plan.newSession || !plan.dropCapabilities || plan.workdir != f.opts.Workdir {
		t.Fatalf("unexpected sandbox settings: %+v", plan)
	}
	for _, operation := range plan.filesystem {
		if operation.kind == filesystemBind && operation.source == f.root {
			t.Fatal("host root exposed")
		}
		if operation.source == f.host("/registry/instance") {
			t.Fatal("host supervisor metadata directory exposed")
		}
	}
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/usr"), dest: "/usr"})
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/etc"), dest: "/etc"})
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemSymlink, source: "usr/bin", dest: "/bin"})
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemSymlink, source: "usr/lib", dest: "/lib64"})
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemProc, dest: "/proc"})
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemDevices, dest: "/dev"})
	for _, path := range []string{"/tmp", "/run", f.opts.Home} {
		requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemTmpfs, dest: path, writable: true})
	}
	shared := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemDirectory, dest: "/tmp/ttc", mode: fs.ModeSticky | 0777})
	private := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemDirectory, dest: "/tmp/ttc/1000", mode: 0700})
	if shared >= private {
		t.Fatal("private scratch directory precedes its parent")
	}
	for _, path := range []string{f.opts.Workdir, f.opts.DataDir, f.opts.CacheDir} {
		requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(path), dest: path, writable: true})
	}
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(f.opts.Executable), dest: f.opts.Executable})
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(f.opts.SocketDir), dest: "/run/ttc-rail", writable: true})
}

func TestSandboxNonMergedSystemDirectories(t *testing.T) {
	f := newMountFixture(t)
	for _, path := range []string{"/bin", "/sbin", "/lib", "/lib64"} {
		if err := os.Remove(f.host(path)); err != nil {
			t.Fatal(err)
		}
		f.dir(t, path)
	}
	plan := f.mustPlan(t)
	for _, path := range []string{"/bin", "/sbin", "/lib", "/lib64"} {
		requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(path), dest: path})
	}
}

func TestSandboxResolverSymlinkDoesNotExposeHostRun(t *testing.T) {
	f := newMountFixture(t)
	f.file(t, "/run/systemd/resolve/resolv.conf")
	f.link(t, "/etc/resolv.conf", "/run/systemd/resolve/resolv.conf")
	plan := f.mustPlan(t)
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/run/systemd/resolve/resolv.conf"), dest: "/run/systemd/resolve/resolv.conf"})
	if slices.Contains(plan.filesystem, filesystemOperation{kind: filesystemBind, source: f.host("/run"), dest: "/run"}) {
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
	plan := f.mustPlan(t)
	for _, path := range paths {
		requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(path), dest: path})
	}
	if slices.Contains(plan.filesystem, filesystemOperation{kind: filesystemBind, source: f.host(f.opts.Home), dest: f.opts.Home, writable: true}) {
		t.Fatal("host home exposed")
	}
}

func TestSandboxDefaultMissingAndExplicitMissing(t *testing.T) {
	f := newMountFixture(t)
	f.mustPlan(t) // No shell, skills or global configuration files are required.
	f.opts.Policy.Mounts = []Mount{{Source: "/missing", Dest: "/extra"}}
	if _, err := f.plan(context.Background()); err == nil || !strings.Contains(err.Error(), "/extra") {
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
	plan := f.mustPlan(t)
	parent := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/other/parent"), dest: "/extra"})
	child := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/other/child"), dest: "/extra/child", writable: true})
	if parent > child {
		t.Fatal("descendant mounted before ancestor")
	}
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/other/bashrc"), dest: "/home/user/.bashrc", writable: true})
	if slices.Contains(plan.filesystem, filesystemOperation{kind: filesystemBind, source: f.host("/home/user/.bashrc"), dest: "/home/user/.bashrc"}) {
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
	plan := f.mustPlan(t)
	usr := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/usr"), dest: "/usr"})
	tool := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/other/tool"), dest: "/usr/bin/tool", writable: true})
	target := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/other/target"), dest: "/target"})
	redirected := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/other/tool"), dest: "/target/nested", writable: true})
	if usr >= tool || target >= redirected {
		t.Fatal("a canonical ancestor hid the descendant overlay")
	}
}

func TestSandboxRejectsAmbiguousCanonicalDestinations(t *testing.T) {
	f := newMountFixture(t)
	f.file(t, "/other/tool")
	f.opts.Policy.Mounts = []Mount{{Source: "/other/tool", Dest: "/bin/tool"}, {Source: "/other/tool", Dest: "/usr/bin/tool"}}
	if _, err := f.plan(context.Background()); err == nil || !strings.Contains(err.Error(), "same path") {
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
	plan := f.mustPlan(t)
	writable := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(f.opts.Workdir), dest: f.opts.Workdir, writable: true})
	readonly := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(config), dest: config})
	if readonly <= writable {
		t.Fatal("mandatory config overlay precedes writable workspace")
	}
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(config), dest: "/alias/settings.json"})
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(config), dest: "/direct"})
}

func TestSandboxAbsoluteSourceSymlinksAreFixtureRooted(t *testing.T) {
	f := newMountFixture(t)
	f.file(t, "/secrets/bashrc")
	f.link(t, "/home/user/.bashrc", "/secrets/bashrc")
	f.opts.Policy.Mounts = []Mount{{Source: "/secrets", Dest: "/allowed", Writable: true}}
	plan := f.mustPlan(t)
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/secrets/bashrc"), dest: "/home/user/.bashrc"})
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/secrets/bashrc"), dest: "/allowed/bashrc"})
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
	plan := f.mustPlan(t)
	denyDir := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemTmpfs, dest: "/extra"})
	denyFile := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/dev/null"), dest: file})
	for _, mount := range []filesystemOperation{
		{kind: filesystemBind, source: f.host("/outside/child"), dest: "/extra/child", writable: true},
		{kind: filesystemBind, source: f.host(file), dest: file},
	} {
		index := requireFilesystemOperation(t, plan, mount)
		if index >= denyDir || index >= denyFile {
			t.Fatal("deny masks must follow all mounts")
		}
	}
	for _, operation := range plan.filesystem {
		if operation.kind == filesystemTmpfs && operation.dest == "/extra/child" {
			t.Fatal("redundant nested deny can reopen denied ancestor")
		}
	}
}

func TestSandboxDenySymlinkMasksItsVisibleTarget(t *testing.T) {
	f := newMountFixture(t)
	f.file(t, filepath.Join(f.opts.Workdir, "secret"))
	f.link(t, filepath.Join(f.opts.Workdir, "link"), "secret")
	f.opts.Policy.Denies = []string{filepath.Join(f.opts.Workdir, "link")}
	requireFilesystemOperation(t, f.mustPlan(t), filesystemOperation{kind: filesystemBind, source: f.host("/dev/null"), dest: filepath.Join(f.opts.Workdir, "secret")})
}

func TestSandboxRejectsWorkspaceAndReservedDenies(t *testing.T) {
	for _, path := range []string{"/home", "/home/user/projects", "/home/user/projects/demo", "/proc", "/dev/null", "/run", "/run/ttc-rail/config"} {
		t.Run(path, func(t *testing.T) {
			f := newMountFixture(t)
			f.opts.Policy.Denies = []string{path}
			if _, err := f.plan(context.Background()); err == nil {
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
			if _, err := f.plan(context.Background()); err == nil {
				t.Fatal("accepted reserved mount")
			}
		})
	}
	f := newMountFixture(t)
	f.link(t, filepath.Join(f.opts.Workdir, "redirect"), "/run/ttc-rail")
	f.opts.Policy.Mounts = []Mount{{Source: "/usr", Dest: filepath.Join(f.opts.Workdir, "redirect")}}
	if _, err := f.plan(context.Background()); err == nil || !strings.Contains(err.Error(), "reserved") {
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
	plan := f.mustPlan(t)
	bind := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/run/docker.sock"), dest: "/run/docker.sock", writable: true})
	deny := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/dev/null"), dest: "/run/docker.sock"})
	if deny <= bind {
		t.Fatal("Docker authorization overrode deny")
	}
	f.env["DOCKER_HOST"] = "tcp://127.0.0.1:2375"
	if _, err := f.plan(context.Background()); err == nil || !strings.Contains(err.Error(), "DOCKER_HOST") {
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
			if _, err := f.plan(context.Background()); err == nil || !strings.Contains(err.Error(), "Docker") {
				t.Fatalf("unauthorized Docker mount accepted: %s rw=%v: %v", source, writable, err)
			}
			f.opts.Policy.Docker = true
			f.mustPlan(t)
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
		if _, err := f.plan(context.Background()); err == nil || !strings.Contains(err.Error(), "socket") {
			t.Fatalf("missing/non-socket Docker source: %v", err)
		}
	}
}

func TestSandboxRequiredPathsAndExecutableInsideWorkdir(t *testing.T) {
	f := newMountFixture(t)
	f.opts.Executable = filepath.Join(f.opts.Workdir, "ttc")
	f.file(t, f.opts.Executable)
	plan := f.mustPlan(t)
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(f.opts.Executable), dest: f.opts.Executable})
	if err := os.RemoveAll(f.host(f.opts.DataDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.plan(context.Background()); err == nil || !strings.Contains(err.Error(), "writable directory") {
		t.Fatalf("missing required writable directory: %v", err)
	}
}

func TestSandboxGlobalConfigSymlinkTargetIsReadonly(t *testing.T) {
	f := newMountFixture(t)
	target := filepath.Join(f.opts.Workdir, "search-settings.json")
	f.file(t, target)
	f.link(t, filepath.Join(f.opts.ConfigHome, "ttc", "web-search.json"), target)
	plan := f.mustPlan(t)
	readonly := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(target), dest: target})
	writable := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(f.opts.Workdir), dest: f.opts.Workdir, writable: true})
	// The initial entrypoint overlay can precede the writable workspace, but
	// alias protection must overlay it again after all writable mounts.
	if !slices.Contains(plan.filesystem[writable+1:], filesystemOperation{kind: filesystemBind, source: f.host(target), dest: target}) {
		t.Fatalf("configuration source was writable after workspace overlay (first RO mount %d)", readonly)
	}
}

func TestSandboxZdotdirHomeDoesNotExposeHostHome(t *testing.T) {
	f := newMountFixture(t)
	f.env["ZDOTDIR"] = f.opts.Home
	f.file(t, filepath.Join(f.opts.Home, ".zshrc"))
	plan := f.mustPlan(t)
	if slices.Contains(plan.filesystem, filesystemOperation{kind: filesystemBind, source: f.host(f.opts.Home), dest: f.opts.Home}) {
		t.Fatal("ZDOTDIR=$HOME exposed the entire host home")
	}
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(filepath.Join(f.opts.Home, ".zshrc")), dest: filepath.Join(f.opts.Home, ".zshrc")})
	for _, value := range []string{"relative", "/", f.opts.Workdir} {
		f.env["ZDOTDIR"] = value
		if _, err := f.plan(context.Background()); err == nil {
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
			if _, err := f.plan(context.Background()); err == nil {
				t.Fatal("accepted invalid sandbox options")
			}
		})
	}
}
