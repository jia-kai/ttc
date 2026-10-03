package rail

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSandboxNeovimDefaultsAndDenies(t *testing.T) {
	f := newMountFixture(t)
	f.opts.ConfigHome = "/settings"
	paths := []string{filepath.Join(f.opts.Home, ".config", "nvim"), "/settings/nvim"}
	for _, path := range paths {
		f.dir(t, path)
	}
	f.opts.Policy.Mounts = []Mount{{Source: "/settings", Dest: "/settings-alias", Writable: true}}
	f.opts.Policy.Denies = []string{paths[0], "/settings/nvim/private"}
	f.file(t, "/settings/nvim/private")
	plan := f.mustPlan(t)
	for _, path := range paths {
		requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(path), dest: path})
	}
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/settings/nvim"), dest: "/settings-alias/nvim"})
	deny := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemTmpfs, dest: paths[0]})
	if deny <= requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(paths[0]), dest: paths[0]}) {
		t.Fatal("Neovim default overrides deny")
	}
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/dev/null"), dest: "/settings/nvim/private"})
}

func TestSandboxZshHistorySelection(t *testing.T) {
	for _, tt := range []struct {
		name, histfile, want string
	}{
		{"unset", "", "/home/user/.zsh_history"},
		{"relative", "history", "/home/user/.zsh_history"},
		{"tilde literal", "~/.history", "/home/user/.zsh_history"},
		{"absolute", "/history/zsh", "/history/zsh"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newMountFixture(t)
			f.file(t, "/home/user/.zsh_history")
			f.file(t, "/history/zsh")
			f.env["HISTFILE"] = tt.histfile
			plan := f.mustPlan(t)
			requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(tt.want), dest: tt.want, writable: true})
			other := "/history/zsh"
			if tt.want == other {
				other = "/home/user/.zsh_history"
			}
			if slices.Contains(plan.filesystem, filesystemOperation{kind: filesystemBind, source: f.host(other), dest: other, writable: true}) {
				t.Fatal("unselected history imported")
			}
		})
	}
}

func TestSandboxZshHistoryMissingNeverCreated(t *testing.T) {
	f := newMountFixture(t)
	f.file(t, "/home/user/.zsh_history")
	f.env["HISTFILE"] = "/history/missing"
	plan := f.mustPlan(t)
	for _, path := range []string{f.env["HISTFILE"], "/home/user/.zsh_history"} {
		if slices.Contains(plan.filesystem, filesystemOperation{kind: filesystemBind, source: f.host(path), dest: path, writable: true}) {
			t.Fatalf("missing absolute HISTFILE fell back or mounted: %s", path)
		}
	}
	if _, err := os.Stat(f.host(f.env["HISTFILE"])); !os.IsNotExist(err) {
		t.Fatalf("history created: %v", err)
	}
}

func TestSandboxZshHistoryDeniesAndSymlinks(t *testing.T) {
	f := newMountFixture(t)
	f.file(t, "/history/actual")
	f.link(t, "/home/user/.zsh_history", "/history/actual")
	f.opts.Policy.Denies = []string{"/home/user/.zsh_history"}
	plan := f.mustPlan(t)
	bind := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/history/actual"), dest: "/home/user/.zsh_history", writable: true})
	deny := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/dev/null"), dest: "/home/user/.zsh_history"})
	if deny <= bind {
		t.Fatal("writable history overrides deny")
	}
}

func TestSandboxZshHistoryRejectsInvalidTypes(t *testing.T) {
	for _, kind := range []string{"directory", "socket", "nul", "reserved"} {
		t.Run(kind, func(t *testing.T) {
			f := newMountFixture(t)
			f.env["HISTFILE"] = "/history/file"
			switch kind {
			case "directory":
				f.dir(t, f.env["HISTFILE"])
			case "socket":
				f.socket(t, f.env["HISTFILE"])
			case "nul":
				f.env["HISTFILE"] = "/history/nul\x00"
			case "reserved":
				f.env["HISTFILE"] = "/dev/null"
			}
			if _, err := f.plan(context.Background()); err == nil || (kind != "reserved" && !strings.Contains(err.Error(), "history")) {
				t.Fatalf("invalid history accepted: %v", err)
			}
		})
	}
}

func TestSandboxGitConfigDefaultAndMissing(t *testing.T) {
	f := newMountFixture(t)
	path := filepath.Join(f.opts.Home, ".gitconfig")
	plan := f.mustPlan(t)
	for _, operation := range plan.filesystem {
		if operation.kind == filesystemBind && operation.dest == path {
			t.Fatal("missing Git config was imported")
		}
	}
	if _, err := os.Lstat(f.host(path)); !os.IsNotExist(err) {
		t.Fatalf("missing Git config was created: %v", err)
	}
	f.file(t, path)
	plan = f.mustPlan(t)
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(path), dest: path})
	if slices.Contains(plan.filesystem, filesystemOperation{kind: filesystemBind, source: f.host(path), dest: path, writable: true}) {
		t.Fatal("default Git config was writable")
	}
}

func TestSandboxMergedSystemWritableDefaultAlias(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "coalesce defaults", true: "reject explicit collision"}[explicit], func(t *testing.T) {
			f := newMountFixture(t)
			f.opts.Workdir = "/usr/bin"
			f.env["ZDOTDIR"] = "/bin"
			if explicit {
				f.opts.Policy.Mounts = []Mount{{Source: "/usr/bin", Dest: "/bin", Writable: true}}
			}
			plan, err := f.plan(context.Background())
			if explicit {
				if err == nil || !strings.Contains(err.Error(), "same path") {
					t.Fatalf("ambiguous explicit destination accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemSymlink, source: "usr/bin", dest: "/bin"})
			requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/usr/bin"), dest: "/usr/bin", writable: true})
			binds := 0
			for _, operation := range plan.filesystem {
				if operation.kind == filesystemBind && operation.dest == "/usr/bin" {
					binds++
				}
			}
			if binds != 1 {
				t.Fatalf("normalized default bind count = %d, want 1", binds)
			}
		})
	}
}

func TestSandboxGitConfigSymlinkProtectsWritableAliases(t *testing.T) {
	f := newMountFixture(t)
	path := filepath.Join(f.opts.Home, ".gitconfig")
	target := filepath.Join(f.opts.Workdir, "git.ini")
	f.file(t, target)
	f.link(t, path, target)
	f.opts.Policy.Mounts = []Mount{{Source: f.opts.Workdir, Dest: "/workspace-alias", Writable: true}}
	plan := f.mustPlan(t)
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(target), dest: path})
	for _, dest := range []string{f.opts.Workdir, "/workspace-alias"} {
		bind := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(f.opts.Workdir), dest: dest, writable: true})
		protected := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(target), dest: filepath.Join(dest, "git.ini")})
		if protected <= bind {
			t.Fatal("Git config protection precedes writable alias")
		}
	}
}

func TestSandboxGitConfigDenyWins(t *testing.T) {
	f := newMountFixture(t)
	path := filepath.Join(f.opts.Home, ".gitconfig")
	f.file(t, path)
	f.opts.Policy.Denies = []string{path}
	plan := f.mustPlan(t)
	bind := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(path), dest: path})
	deny := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/dev/null"), dest: path})
	if deny <= bind {
		t.Fatal("default Git config overrides deny")
	}
}

func TestSandboxGitConfigHardlinksFailClosed(t *testing.T) {
	f := newMountFixture(t)
	path := filepath.Join(f.opts.Home, ".gitconfig")
	f.file(t, path)
	if err := os.Link(f.host(path), f.host(filepath.Join(f.opts.Workdir, "git-alias"))); err != nil {
		t.Fatal(err)
	}
	_, err := f.plan(context.Background())
	if err == nil || !strings.Contains(err.Error(), "protected file") || !strings.Contains(err.Error(), "multiple hardlinks") {
		t.Fatalf("hardlinked Git config accepted: %v", err)
	}
}

func TestSandboxNeovimWorkspaceAliasUsable(t *testing.T) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("Bubblewrap is optional outside rail runtime")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, bwrap, "--unshare-user", "--unshare-pid", "--ro-bind", "/", "/", "--", "/bin/true").CombinedOutput(); err != nil {
		t.Skipf("Bubblewrap namespaces unavailable: %v: %s", err, output)
	}
	root := t.TempDir()
	opts := sandboxOptions{
		Workdir: filepath.Join(root, "project"), Home: filepath.Join(root, "home"),
		DataDir: filepath.Join(root, "data"), CacheDir: filepath.Join(root, "cache"),
		ConfigHome: filepath.Join(root, "home", ".config"), SocketDir: filepath.Join(root, "control"),
		Hostname: "fixture",
	}
	opts.Executable, err = os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{opts.Workdir, opts.Home, opts.DataDir, opts.CacheDir, opts.ConfigHome, opts.SocketDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(opts.ConfigHome, "nvim")
	if err := os.Symlink(opts.Workdir, alias); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(opts.Workdir, "init.lua")
	if err := os.WriteFile(config, []byte("neovim-fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	policyFile := filepath.Join(opts.Workdir, "ttc-rail.json")
	if err := os.WriteFile(policyFile, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	opts.Policy.ConfigFiles = []string{policyFile}
	spec, err := resolveSandboxSpec(ctx, opts, sandboxEnvironment{Path: "/usr/bin:/bin", UID: os.Geteuid()})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planSandbox(ctx, spec, "/")
	if err != nil {
		t.Fatal(err)
	}
	// A command in the real namespace can read Neovim's expected path, write
	// through that alias, and cannot modify the mandatory policy through it.
	script := `test "$(cat "$1/init.lua")" = neovim-fixture && printf alias-write > "$1/alias-marker" && ! (printf BAD > "$1/ttc-rail.json")`
	command, err := bubblewrapInvocation(*plan, sandboxProcess{command: []string{"/bin/sh", "-c", script, "sh", alias}, environment: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, command.executable, command.args...)
	cmd.Env = command.environment
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("forwarded Neovim alias unusable: %v: %s", err, output)
	}
	if data, err := os.ReadFile(filepath.Join(opts.Workdir, "alias-marker")); err != nil || string(data) != "alias-write" {
		t.Fatalf("alias did not write selected workspace: %q, %v", data, err)
	}
	if data, err := os.ReadFile(policyFile); err != nil || string(data) != "{}" {
		t.Fatalf("mandatory policy was modified: %q, %v", data, err)
	}
}
