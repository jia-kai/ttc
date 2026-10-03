//go:build linux

package rail

import (
	"context"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestSandboxSpecPureCapturedInputsAndOwnership(t *testing.T) {
	// None of these required sources exist. Specification construction must not
	// inspect them, canonicalize them, or consult the live process environment.
	root := filepath.Join(t.TempDir(), "absent")
	opts := sandboxOptions{
		Workdir: filepath.Join(root, "work"), Home: filepath.Join(root, "home"),
		DataDir: filepath.Join(root, "data"), CacheDir: filepath.Join(root, "cache"),
		ConfigHome: filepath.Join(root, "config"), Executable: filepath.Join(root, "bin/ttc"),
		SocketDir: filepath.Join(root, "control"), Hostname: "captured-host",
		Policy: Policy{
			Mounts: []Mount{{Source: filepath.Join(root, "source"), Dest: "/explicit", Writable: true}},
			Denies: []string{"/explicit/private"}, ConfigFiles: []string{filepath.Join(root, "rail.json")},
			Docker: true, SSHAgent: true,
		},
	}
	captured := []string{
		"PATH=/captured/bin", "SSH_AUTH_SOCK=" + filepath.Join(root, "agent.sock"),
		"DOCKER_HOST=unix:///var/run/docker.sock", "HISTFILE=" + filepath.Join(root, "history"),
		"ZDOTDIR=" + filepath.Join(root, "zsh"), "UNRELATED=ignored",
	}
	uid := os.Geteuid() + 123
	env := planningEnvironment(captured, uid)
	wantEnv := sandboxEnvironment{
		Path: "/captured/bin", SSHAuthSock: filepath.Join(root, "agent.sock"),
		DockerHost: "unix:///var/run/docker.sock", HistoryFile: filepath.Join(root, "history"),
		ZDotDir: filepath.Join(root, "zsh"), UID: uid,
	}
	if env != wantEnv {
		t.Fatalf("captured environment = %#v, want %#v", env, wantEnv)
	}
	for _, name := range []string{"PATH", "SSH_AUTH_SOCK", "DOCKER_HOST", "HISTFILE", "ZDOTDIR"} {
		t.Setenv(name, "invalid-relative-live-value")
	}
	spec, err := resolveSandboxSpec(context.Background(), opts, env)
	if err != nil {
		t.Fatal(err)
	}
	wantExplicit := mountRequest{Source: opts.Policy.Mounts[0].Source, Dest: "/explicit", Writable: true, Required: true, Origin: mountExplicit}
	wantRequests := []mountRequest{
		wantExplicit,
		{Source: "/var/run/docker.sock", Dest: "/var/run/docker.sock", Writable: true, Required: true, Origin: mountService},
		{Source: wantEnv.SSHAuthSock, Dest: sshAgentDest, Writable: true, Required: true, Origin: mountService},
		{Source: wantEnv.HistoryFile, Dest: wantEnv.HistoryFile, Writable: true, Kind: mountHistory},
		{Source: wantEnv.ZDotDir, Dest: wantEnv.ZDotDir, Protect: true},
	}
	wantDenies := append([]string(nil), opts.Policy.Denies...)
	wantConfigs := append([]string(nil), opts.Policy.ConfigFiles...)
	// Mutate both backing arrays and value inputs after construction.
	opts.Policy.Mounts[0] = Mount{Source: "/changed", Dest: "/changed"}
	opts.Policy.Denies[0] = "/changed"
	opts.Policy.ConfigFiles[0] = "/changed"
	opts.Policy.SSHAgent, opts.Policy.Docker = false, false
	opts.Workdir = "/changed"
	captured[0] = "PATH=/changed"
	env.Path, env.SSHAuthSock, env.UID = "/changed", "/changed", 0
	if spec.environment != wantEnv || spec.options.Workdir == opts.Workdir || !spec.options.Policy.SSHAgent || !spec.options.Policy.Docker {
		t.Fatalf("spec lost captured values: %#v", spec)
	}
	if spec.options.Policy.Mounts != nil || !reflect.DeepEqual(spec.options.Policy.Denies, wantDenies) || !reflect.DeepEqual(spec.options.Policy.ConfigFiles, wantConfigs) {
		t.Fatalf("spec policy does not own consumed/copied data: %#v", spec.options.Policy)
	}
	for _, wanted := range wantRequests {
		found := false
		for _, request := range spec.mounts {
			found = found || request == wanted
		}
		if !found {
			t.Fatalf("missing captured/provenance request %#v in %#v", wanted, spec.mounts)
		}
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("specification created or required absent sources: %v", err)
	}
}

func TestSandboxPlanSeparatesHostSourcesAndSandboxDestinations(t *testing.T) {
	f := newMountFixture(t)
	f.file(t, "/sources/tool")
	f.link(t, "/sources/tool-alias", "/sources/tool")
	f.dir(t, "/sources/parent")
	f.link(t, "/sources/parent/link", "/target/nested")
	f.dir(t, "/sources/target")
	// The host's destination path disagrees with the namespace established by
	// the parent bind. It must never determine the sandbox overlay destination.
	f.link(t, "/extra/link", "/host-only/wrong")
	f.opts.Policy.Mounts = []Mount{
		{Source: "/sources/tool-alias", Dest: "/extra/link", Writable: true},
		{Source: "/sources/parent", Dest: "/extra"},
		{Source: "/sources/target", Dest: "/target"},
		{Source: "/sources/tool-alias", Dest: "/bin/tool", Writable: true},
	}
	plan, err := f.plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	parent := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/sources/parent"), dest: "/extra"})
	target := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/sources/target"), dest: "/target"})
	redirect := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/sources/tool"), dest: "/target/nested", writable: true})
	usr := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/usr"), dest: "/usr"})
	tool := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/sources/tool"), dest: "/usr/bin/tool", writable: true})
	if redirect <= parent || redirect <= target || tool <= usr {
		t.Fatalf("overlays precede namespace ancestors: parent=%d target=%d redirected=%d usr=%d tool=%d", parent, target, redirect, usr, tool)
	}
	for _, operation := range plan.filesystem {
		if operation.kind == filesystemBind && (operation.dest == "/extra/link" || operation.dest == "/bin/tool" || strings.HasPrefix(operation.dest, "/host-only")) {
			t.Fatalf("unresolved or host-resolved sandbox destination: %#v", operation)
		}
	}
}

func TestSandboxPlanSyntheticAliasHiddenByAncestorBind(t *testing.T) {
	for _, separateRunImport := range []bool{false, true} {
		t.Run(strconv.FormatBool(separateRunImport), func(t *testing.T) {
			f := newMountFixture(t)
			f.dir(t, "/sources/var")
			f.link(t, "/sources/var/run", "/alternate")
			f.dir(t, "/sources/alternate")
			f.file(t, "/sources/tool")
			f.opts.Policy.Mounts = []Mount{
				{Source: "/sources/var", Dest: "/var"},
				{Source: "/sources/alternate", Dest: "/alternate"},
				{Source: "/sources/tool", Dest: "/var/run/tool"},
			}
			if separateRunImport {
				f.file(t, "/sources/other")
				f.opts.Policy.Mounts = append(f.opts.Policy.Mounts, Mount{Source: "/sources/other", Dest: "/run/tool", Writable: true})
			}
			plan := f.mustPlan(t)
			parent := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/sources/var"), dest: "/var"})
			target := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/sources/alternate"), dest: "/alternate"})
			tool := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/sources/tool"), dest: "/alternate/tool"})
			if tool <= parent || tool <= target {
				t.Fatalf("redirected import precedes its namespace ancestors: parent=%d target=%d tool=%d", parent, target, tool)
			}
			for _, operation := range plan.filesystem {
				if operation.kind == filesystemBind && operation.source == f.host("/sources/tool") && operation.dest != "/alternate/tool" {
					t.Fatalf("import retained hidden synthetic alias: %+v", operation)
				}
			}
			if separateRunImport {
				requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/sources/other"), dest: "/run/tool", writable: true})
			}
		})
	}
}

func TestSandboxPlanRejectsCollisionWithPlacedPrimaryMount(t *testing.T) {
	for _, writable := range []bool{false, true} {
		t.Run(strconv.FormatBool(writable), func(t *testing.T) {
			f := newMountFixture(t)
			f.file(t, "/sources/first")
			f.file(t, "/sources/second")
			f.dir(t, "/sources/z")
			f.link(t, "/sources/z/link", "/a")
			f.opts.Policy.Mounts = []Mount{
				{Source: "/sources/first", Dest: "/a"},
				{Source: "/sources/z", Dest: "/z"},
				{Source: "/sources/second", Dest: "/z/link", Writable: writable},
			}
			plan, err := f.plan(context.Background())
			if err == nil || !strings.Contains(err.Error(), "same path") || plan != nil {
				t.Fatalf("ambiguous primary destination accepted: plan=%+v err=%v", plan, err)
			}
		})
	}
}

func TestSandboxPlanLateCanonicalParentPreservesChildAccess(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(strconv.FormatBool(directory), func(t *testing.T) {
			f := newMountFixture(t)
			if directory {
				f.dir(t, "/sources/child")
				f.dir(t, "/sources/parent/child")
			} else {
				f.file(t, "/sources/child")
				f.file(t, "/sources/parent/child")
			}
			f.dir(t, "/sources/z")
			f.link(t, "/sources/z/link", "/a")
			f.opts.Policy.Mounts = []Mount{
				{Source: "/sources/child", Dest: "/a/child"},
				{Source: "/sources/z", Dest: "/z"},
				{Source: "/sources/parent", Dest: "/z/link", Writable: true},
			}
			plan := f.mustPlan(t)
			childOperation := filesystemOperation{kind: filesystemBind, source: f.host("/sources/child"), dest: "/a/child"}
			child := requireFilesystemOperation(t, plan, childOperation)
			parent := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/sources/parent"), dest: "/a", writable: true})
			if child <= parent {
				t.Fatalf("late writable parent hides read-only child: parent=%d child=%d", parent, child)
			}
			count := 0
			for _, operation := range plan.filesystem {
				if operation == childOperation {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("tentative child operations leaked into final plan: count=%d", count)
			}
		})
	}
}

func TestSandboxPlanAncestorReplacementHidesDescendantLoop(t *testing.T) {
	for _, canonicalAncestor := range []bool{false, true} {
		t.Run(strconv.FormatBool(canonicalAncestor), func(t *testing.T) {
			f := newMountFixture(t)
			f.dir(t, "/sources/tree/branch")
			f.link(t, "/sources/tree/branch/bad", "bad")
			f.dir(t, "/sources/branch/bad")
			f.file(t, "/sources/leaf")
			branchDest := "/tree/branch"
			if canonicalAncestor {
				f.dir(t, "/sources/z")
				f.link(t, "/sources/z/link", branchDest)
				f.opts.Policy.Mounts = append(f.opts.Policy.Mounts, Mount{Source: "/sources/z", Dest: "/z"})
				branchDest = "/z/link"
			}
			f.opts.Policy.Mounts = append(f.opts.Policy.Mounts,
				Mount{Source: "/sources/tree", Dest: "/tree"},
				Mount{Source: "/sources/branch", Dest: branchDest},
				Mount{Source: "/sources/leaf", Dest: "/tree/branch/bad/leaf"},
			)
			plan := f.mustPlan(t)
			tree := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/sources/tree"), dest: "/tree"})
			branch := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/sources/branch"), dest: "/tree/branch"})
			leaf := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/sources/leaf"), dest: "/tree/branch/bad/leaf"})
			if tree >= branch || branch >= leaf {
				t.Fatalf("replacement ancestors are out of order: tree=%d branch=%d leaf=%d", tree, branch, leaf)
			}
		})
	}
}

func TestSandboxPlanDeniesCompareCanonicalWorkdir(t *testing.T) {
	for _, deny := range []string{"/redirected-workdir", "/home/user/projects/demo", "/redirected-workdir/private"} {
		t.Run(deny, func(t *testing.T) {
			f := newMountFixture(t)
			f.dir(t, "/sources/projects")
			f.link(t, "/sources/projects/demo", "/redirected-workdir")
			f.dir(t, filepath.Join(f.opts.Workdir, "private"))
			f.opts.Policy.Mounts = []Mount{{Source: "/sources/projects", Dest: "/home/user/projects"}}
			f.opts.Policy.Denies = []string{deny}
			plan, err := f.plan(context.Background())
			if deny != "/redirected-workdir/private" {
				if err == nil || !strings.Contains(err.Error(), "covers writable workdir") || plan != nil {
					t.Fatalf("whole canonical workdir deny accepted: plan=%+v err=%v", plan, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if plan.workdir != "/redirected-workdir" {
				t.Fatalf("planned workdir retained its lexical alias: %q", plan.workdir)
			}
			requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemTmpfs, dest: deny})
			if !strings.Contains(tmuxStartup(*plan), "new-session -d -s rail -c '/redirected-workdir'\n") {
				t.Fatal("tmux startup did not use the canonical planned workdir")
			}
		})
	}
}

func TestSandboxPlanWritableAliasesProtectionAndMaskOrdering(t *testing.T) {
	f := newMountFixture(t)
	config := filepath.Join(f.opts.Workdir, "rail.json")
	f.file(t, config)
	f.dir(t, filepath.Join(f.opts.Workdir, "secret"))
	defaultAlias := filepath.Join(f.opts.ConfigHome, "nvim")
	f.link(t, defaultAlias, f.opts.Workdir)
	f.opts.Policy.Mounts = []Mount{{Source: f.opts.Workdir, Dest: "/workspace-alias", Writable: true}}
	f.opts.Policy.ConfigFiles = []string{config}
	f.opts.Policy.Denies = []string{"/workspace-alias/rail.json", "/workspace-alias/secret"}
	plan, err := f.plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mandatory := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(config), dest: config})
	for _, dest := range []string{f.opts.Workdir, defaultAlias, "/workspace-alias"} {
		primary := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(f.opts.Workdir), dest: dest, writable: true})
		protection := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(config), dest: filepath.Join(dest, "rail.json")})
		if primary >= mandatory || primary >= protection {
			t.Fatalf("protection precedes writable root/default/explicit alias %s: primary=%d mandatory=%d protection=%d", dest, primary, mandatory, protection)
		}
		if dest != f.opts.Workdir && protection <= mandatory {
			t.Fatalf("alias protection precedes mandatory config overlay: %s", dest)
		}
	}
	fileMask := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/dev/null"), dest: "/workspace-alias/rail.json"})
	directoryMask := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemTmpfs, dest: "/workspace-alias/secret"})
	for i, operation := range plan.filesystem {
		if operation.kind == filesystemBind && operation.source != f.host("/dev/null") && (i >= fileMask || i >= directoryMask) {
			t.Fatalf("primary/protection bind follows a deny mask: index=%d operation=%#v", i, operation)
		}
	}
	if got := plan.environment[len(plan.environment)-1]; got != (environmentChange{name: "SSH_AUTH_SOCK", unset: true}) {
		t.Fatalf("disabled SSH service must remove inherited socket environment: %#v", got)
	}
}

func TestSandboxPlanCapturedEnvironmentAndExactSyntheticModes(t *testing.T) {
	f := newMountFixture(t)
	f.dir(t, "/tmp")
	agent, err := net.Listen("unix", f.host("/tmp/captured-agent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Close() })
	f.file(t, "/history/captured")
	f.dir(t, "/captured-zsh")
	f.opts.Policy.SSHAgent = true
	uid := os.Geteuid() + 123
	env := planningEnvironment([]string{
		"PATH=/captured/bin:/usr/bin", "SSH_AUTH_SOCK=/tmp/captured-agent.sock",
		"HISTFILE=/history/captured", "ZDOTDIR=/captured-zsh",
	}, uid)
	spec, err := resolveSandboxSpec(context.Background(), f.opts, env)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"PATH", "SSH_AUTH_SOCK", "DOCKER_HOST", "HISTFILE", "ZDOTDIR"} {
		t.Setenv(name, "invalid-relative-live-value")
	}
	plan, err := planSandbox(context.Background(), spec, f.root)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := []filesystemOperation{
		{kind: filesystemProc, dest: "/proc"},
		{kind: filesystemDevices, dest: "/dev"},
		{kind: filesystemTmpfs, dest: "/tmp", writable: true},
		{kind: filesystemDirectory, dest: "/tmp/ttc", mode: fs.ModeSticky | 0777},
		{kind: filesystemDirectory, dest: filepath.Join("/tmp/ttc", strconv.Itoa(uid)), mode: 0700},
		{kind: filesystemTmpfs, dest: "/run", writable: true},
		{kind: filesystemTmpfs, dest: f.opts.Home, writable: true},
	}
	if len(plan.filesystem) < len(wantPrefix) || !reflect.DeepEqual(plan.filesystem[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("synthetic filesystem prefix = %#v, want %#v", plan.filesystem, wantPrefix)
	}
	wantEnvironment := []environmentChange{
		{name: "PATH", value: "/opt:/captured/bin:/usr/bin"},
		{name: "TTC_DATA_DIR", value: f.opts.DataDir},
		{name: "XDG_RUNTIME_DIR", value: "/run/ttc-rail"},
		{name: "SSH_AUTH_SOCK", value: sshAgentDest},
	}
	if !reflect.DeepEqual(plan.environment, wantEnvironment) {
		t.Fatalf("planned environment = %#v, want %#v", plan.environment, wantEnvironment)
	}
	if plan.hostname != "host-ttc" || plan.workdir != f.opts.Workdir || !plan.newSession || !plan.dropCapabilities || !reflect.DeepEqual(plan.namespaces, []namespaceKind{namespaceUser, namespaceProcess, namespaceHostname}) {
		t.Fatalf("planned isolation fields = %#v", plan)
	}
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/tmp/captured-agent.sock"), dest: sshAgentDest, writable: true})
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/history/captured"), dest: "/history/captured", writable: true})
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/captured-zsh"), dest: "/captured-zsh"})
}

func TestSandboxPlanFreezesTmuxConfigSelection(t *testing.T) {
	f := newMountFixture(t)
	xdgConfig := filepath.Join(f.opts.ConfigHome, "tmux/tmux.conf")
	homeConfig := filepath.Join(f.opts.Home, ".tmux.conf")
	f.file(t, xdgConfig)
	xdgPlan, err := f.plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if xdgPlan.tmuxConfig != xdgConfig {
		t.Fatalf("planned XDG tmux config = %q, want %q", xdgPlan.tmuxConfig, xdgConfig)
	}
	// A new home config changes the next plan, not a previously resolved plan.
	f.file(t, homeConfig)
	homePlan, err := f.plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if homePlan.tmuxConfig != homeConfig || xdgPlan.tmuxConfig != xdgConfig {
		t.Fatalf("tmux selection is not plan-owned: home=%q xdg=%q", homePlan.tmuxConfig, xdgPlan.tmuxConfig)
	}
	for _, plan := range []*sandboxPlan{xdgPlan, homePlan} {
		startup := tmuxStartup(*plan)
		if !strings.Contains(startup, "source-file "+tmuxQuote(plan.tmuxConfig)+"\n") || !strings.Contains(startup, "new-session -d -s rail -c "+tmuxQuote(plan.workdir)+"\n") {
			t.Fatalf("startup did not consume planned config/workdir: %q", startup)
		}
	}
	xdgStartup, homeStartup := tmuxStartup(*xdgPlan), tmuxStartup(*homePlan)
	if err := os.RemoveAll(f.root); err != nil {
		t.Fatal(err)
	}
	f.opts.Workdir = "/changed"
	if got := tmuxStartup(*xdgPlan); got != xdgStartup {
		t.Fatalf("XDG startup reread deleted config: %q, want %q", got, xdgStartup)
	}
	if got := tmuxStartup(*homePlan); got != homeStartup {
		t.Fatalf("home startup reread deleted config: %q, want %q", got, homeStartup)
	}
}
