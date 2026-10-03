package rail

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadPolicySSHAgent(t *testing.T) {
	for _, tt := range []struct {
		name, global, project string
		enabled, invalid      bool
	}{
		{"authorized only", `{"authorize_services":["ssh-agent"]}`, `{}`, false, false},
		{"global request", `{"authorize_services":["ssh-agent"],"allow":["ssh-agent"]}`, `{}`, true, false},
		{"project request", `{"authorize_services":["ssh-agent"]}`, `{"allow":["ssh-agent"]}`, true, false},
		{"unauthorized", `{}`, `{"allow":["ssh-agent"]}`, false, true},
		{"global unauthorized", `{"allow":["ssh-agent"]}`, `{}`, false, true},
		{"global deny", `{"authorize_services":["ssh-agent"],"deny":["ssh-agent"]}`, `{"allow":["ssh-agent"]}`, false, false},
		{"project deny", `{"authorize_services":["ssh-agent"],"allow":["ssh-agent"]}`, `{"deny":["ssh-agent"]}`, false, false},
		{"deny only", `{}`, `{"deny":["ssh-agent"]}`, false, false},
		{"denied unauthorized request", `{"deny":["ssh-agent"]}`, `{"allow":["ssh-agent"]}`, false, true},
		{"project authorization", `{}`, `{"authorize_services":["ssh-agent"]}`, false, true},
		{"independent authorization", `{"authorize_services":["docker"]}`, `{"allow":["ssh-agent"]}`, false, true},
		{"ordinary service path", `{}`, `{"allow":[{"source":"ssh-agent"}]}`, false, false},
		{"both", `{"authorize_services":["docker","ssh-agent"]}`, `{"allow":["docker","ssh-agent"]}`, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newPolicyFixture(t)
			writePolicyConfig(t, f.global, tt.global)
			writePolicyConfig(t, f.project, tt.project)
			p, err := LoadPolicy(f.home, f.configHome, f.workdir)
			if (err != nil) != tt.invalid || p.SSHAgent != tt.enabled {
				t.Fatalf("policy=%#v error=%v", p, err)
			}
			if tt.name == "both" && !p.Docker {
				t.Fatal("SSH authorization affected Docker")
			}
		})
	}
}

func TestAllowSSHAuthSock(t *testing.T) {
	for _, tt := range []struct {
		config         string
		allow, invalid bool
	}{
		{"", false, false}, {`{}`, false, false},
		{`{"ttc_allow_ssh_auth_sock":true}`, true, false},
		{`{"ttc_allow_ssh_auth_sock":false}`, false, false},
		{`{"authorize_services":["ssh-agent"]}`, false, false},
		{`{"ttc_allow_ssh_auth_sock":null}`, false, true},
		{`{"ttc_allow_ssh_auth_sock":1}`, false, true},
		{`{"ttc_allow_ssh_auth_sock":"true"}`, false, true},
		{`{"ttc_allow_ssh_auth_sock":[]}`, false, true},
		{`{"ttc_allow_ssh_auth_sock":{}}`, false, true},
		{`{"ttc_allow_ssh_auth_sock":true,"ttc_allow_ssh_auth_sock":false}`, false, true},
		{`{"ttc_allow_ssh_auth_sock":true,"unknown":[]}`, false, true},
		{`{"ttc_allow_ssh_auth_sock":true,"allow":null}`, false, true},
		{`{"ttc_allow_ssh_auth_sock":true} {}`, false, true},
	} {
		t.Run(tt.config, func(t *testing.T) {
			f := newPolicyFixture(t)
			if tt.config != "" {
				writePolicyConfig(t, f.global, tt.config)
			}
			// The runtime helper neither reads nor validates project config.
			writePolicyConfig(t, f.project, `invalid project`)
			allow, err := AllowSSHAuthSock(f.home, f.configHome)
			if (err != nil) != tt.invalid || allow != tt.allow {
				t.Fatalf("allow=%v error=%v", allow, err)
			}
			if err != nil && !strings.Contains(err.Error(), f.global) {
				t.Fatalf("missing config context: %v", err)
			}
		})
	}
	for _, value := range []string{"true", "false", "null"} {
		f := newPolicyFixture(t)
		writePolicyConfig(t, f.project, `{"ttc_allow_ssh_auth_sock":`+value+`}`)
		if _, err := LoadPolicy(f.home, f.configHome, f.workdir); err == nil {
			t.Fatalf("project setting %s accepted", value)
		}
	}
}

func TestAllowSSHAuthSockRootsAndFileValidation(t *testing.T) {
	f := newPolicyFixture(t)
	for _, roots := range [][2]string{{"", f.configHome}, {"relative", f.configHome}, {f.home, "relative"}, {f.home + "\x00", ""}, {f.home, f.configHome + "\x00"}} {
		if _, err := AllowSSHAuthSock(roots[0], roots[1]); err == nil {
			t.Fatalf("invalid roots accepted: %q", roots)
		}
	}
	fallback := filepath.Join(f.home, ".config", "ttc", "rail.json")
	if err := os.MkdirAll(filepath.Dir(fallback), 0700); err != nil {
		t.Fatal(err)
	}
	writePolicyConfig(t, fallback, `{"ttc_allow_ssh_auth_sock":true}`)
	if allow, err := AllowSSHAuthSock(f.home, ""); err != nil || !allow {
		t.Fatalf("fallback: allow=%v error=%v", allow, err)
	}
	if err := os.Symlink(fallback, f.global); err != nil {
		t.Fatal(err)
	}
	if _, err := AllowSSHAuthSock(f.home, f.configHome); err == nil {
		t.Fatal("accepted symlink config")
	}
	if err := os.Remove(f.global); err != nil {
		t.Fatal(err)
	}
	writePolicyConfig(t, f.global, `{}`+strings.Repeat(" ", maxConfigBytes))
	if _, err := AllowSSHAuthSock(f.home, f.configHome); err == nil {
		t.Fatal("accepted oversized config")
	}
}

func (f *mountFixture) socket(t *testing.T, path string) {
	t.Helper()
	f.dir(t, filepath.Dir(path))
	listener, err := net.Listen("unix", f.host(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
}

func TestSandboxSSHAgentMountAndEnvironment(t *testing.T) {
	f := newMountFixture(t)
	f.env["SSH_AUTH_SOCK"] = "/tmp/agent/socket"
	f.socket(t, f.env["SSH_AUTH_SOCK"])
	requireEnvironmentChange(t, f.mustPlan(t), environmentChange{name: "SSH_AUTH_SOCK", unset: true})
	f.opts.Policy.SSHAgent = true
	plan := f.mustPlan(t)
	requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(f.env["SSH_AUTH_SOCK"]), dest: sshAgentDest, writable: true})
	requireEnvironmentChange(t, plan, environmentChange{name: "SSH_AUTH_SOCK", value: sshAgentDest})
	if slices.Contains(plan.environment, environmentChange{name: "SSH_AUTH_SOCK", unset: true}) {
		t.Fatal("enabled agent environment was unset")
	}
	f.link(t, "/agent-link", "/tmp/agent/socket")
	f.env["SSH_AUTH_SOCK"] = "/agent-link"
	requireFilesystemOperation(t, f.mustPlan(t), filesystemOperation{kind: filesystemBind, source: f.host("/tmp/agent/socket"), dest: sshAgentDest, writable: true})
}

func TestSandboxSSHAgentInvalidEndpoints(t *testing.T) {
	for _, path := range []string{"", "relative", "/missing", "/regular", "/directory", "/nul\x00"} {
		t.Run(path, func(t *testing.T) {
			f := newMountFixture(t)
			f.file(t, "/regular")
			f.dir(t, "/directory")
			f.env["SSH_AUTH_SOCK"] = path
			f.opts.Policy.SSHAgent = true
			if _, err := f.plan(context.Background()); err == nil || !strings.Contains(err.Error(), "SSH_AUTH_SOCK") {
				t.Fatalf("invalid endpoint accepted: %v", err)
			}
		})
	}
}

func TestSSHAgentOrdinaryMountGate(t *testing.T) {
	for _, source := range []string{"/tmp/agent/socket", "/tmp/agent", "/agent-link", "/agent-dir-link"} {
		for _, writable := range []bool{false, true} {
			t.Run(source+map[bool]string{false: " ro", true: " rw"}[writable], func(t *testing.T) {
				f := newMountFixture(t)
				f.env["SSH_AUTH_SOCK"] = "/tmp/agent/socket"
				f.socket(t, f.env["SSH_AUTH_SOCK"])
				f.link(t, "/agent-link", "/tmp/agent/socket")
				f.link(t, "/agent-dir-link", "/tmp/agent")
				f.opts.Policy.Mounts = []Mount{{Source: source, Dest: "/extra", Writable: writable}}
				if _, err := f.plan(context.Background()); err == nil || !strings.Contains(err.Error(), "globally authorized ssh-agent") {
					t.Fatalf("agent source exposed: %v", err)
				}
				f.opts.Policy.SSHAgent = true
				f.mustPlan(t)
			})
		}
	}
}

func TestSSHAgentDefaultMountGate(t *testing.T) {
	for _, location := range []string{"workdir", "data", "cache", "nvim", "ttc config", "socket dir", "system"} {
		t.Run(location, func(t *testing.T) {
			f := newMountFixture(t)
			parent := map[string]string{"workdir": f.opts.Workdir, "data": f.opts.DataDir, "cache": f.opts.CacheDir, "nvim": filepath.Join(f.opts.ConfigHome, "nvim"), "ttc config": filepath.Join(f.opts.ConfigHome, "ttc"), "socket dir": f.opts.SocketDir, "system": "/etc"}[location]
			f.env["SSH_AUTH_SOCK"] = filepath.Join(parent, "agent.sock")
			f.socket(t, f.env["SSH_AUTH_SOCK"])
			if _, err := f.plan(context.Background()); err == nil || !strings.Contains(err.Error(), "SSH_AUTH_SOCK") {
				t.Fatalf("default exposes agent: %v", err)
			}
			f.opts.Policy.SSHAgent = true
			f.mustPlan(t)
		})
	}
}

func TestSSHAgentDenyMasksServiceAndAliases(t *testing.T) {
	for _, deny := range []string{"/tmp/agent/socket", "/tmp/agent", sshAgentDest, "/agent-link"} {
		t.Run(deny, func(t *testing.T) {
			f := newMountFixture(t)
			f.env["SSH_AUTH_SOCK"] = "/agent-link"
			f.socket(t, "/tmp/agent/socket")
			f.link(t, "/agent-link", "/tmp/agent/socket")
			f.opts.Policy.SSHAgent = true
			f.opts.Policy.Mounts = []Mount{{Source: "/tmp/agent", Dest: "/alias"}, {Source: "/agent-link", Dest: "/direct", Writable: true}}
			f.opts.Policy.Denies = []string{deny}
			plan := f.mustPlan(t)
			requireEnvironmentChange(t, plan, environmentChange{name: "SSH_AUTH_SOCK", unset: true})
			for _, dest := range []string{sshAgentDest, "/alias/socket", "/direct"} {
				mask := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/dev/null"), dest: dest})
				if mask <= requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/tmp/agent/socket"), dest: sshAgentDest, writable: true}) {
					t.Fatal("deny precedes service mount")
				}
			}
		})
	}
}

func TestSSHAgentDeniedServiceCannotExposeAlias(t *testing.T) {
	f := newMountFixture(t)
	f.socket(t, "/tmp/agent/socket")
	f.env["SSH_AUTH_SOCK"] = "/tmp/agent/socket"
	parsed, err := parsePolicy([]byte(`{"authorize_services":["ssh-agent"],"allow":["ssh-agent"],"deny":["ssh-agent"]}`), "/settings", f.opts.Home, true)
	if err != nil {
		t.Fatal(err)
	}
	f.opts.Policy.SSHAgent = parsed.requests["ssh-agent"] && !parsed.deniedServices["ssh-agent"]
	f.opts.Policy.Mounts = []Mount{{Source: "/tmp/agent", Dest: "/alias"}}
	if _, err := f.plan(context.Background()); err == nil {
		t.Fatal("denied service bypassed with directory alias")
	}
}

func TestSSHAgentDenyResolvesHostEndpointAliases(t *testing.T) {
	for _, deny := range []string{"/var/run/agent.sock", "/host-agent-link"} {
		t.Run(deny, func(t *testing.T) {
			f := newMountFixture(t)
			f.socket(t, "/run/agent.sock")
			f.env["SSH_AUTH_SOCK"] = "/run/agent.sock"
			f.link(t, "/host-agent-link", "/run/agent.sock")
			f.opts.Policy.SSHAgent = true
			f.opts.Policy.Denies = []string{deny}
			plan := f.mustPlan(t)
			requireEnvironmentChange(t, plan, environmentChange{name: "SSH_AUTH_SOCK", unset: true})
			requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/dev/null"), dest: sshAgentDest})
		})
	}
}

func TestSSHAgentDisabledInvalidAndMissingEnvironment(t *testing.T) {
	f := newMountFixture(t)
	for _, value := range []string{"", "/missing-agent"} {
		f.env["SSH_AUTH_SOCK"] = value
		requireEnvironmentChange(t, f.mustPlan(t), environmentChange{name: "SSH_AUTH_SOCK", unset: true})
	}
	// Relative endpoints cannot be audited against absolute mount sources, so
	// reject them even when forwarding was not requested instead of exposing one.
	f.env["SSH_AUTH_SOCK"] = "relative"
	if _, err := f.plan(context.Background()); err == nil {
		t.Fatal("relative disabled endpoint accepted")
	}
}

func TestSSHAgentServiceAndTTCPolicyAreIndependent(t *testing.T) {
	f := newPolicyFixture(t)
	writePolicyConfig(t, f.global, `{"ttc_allow_ssh_auth_sock":true}`)
	if p := loadFixturePolicy(t, f); p.SSHAgent || p.Docker {
		t.Fatalf("TTC environment policy enabled a rail service: %#v", p)
	}
	writePolicyConfig(t, f.global, `{"authorize_services":["ssh-agent"],"allow":["ssh-agent"],"ttc_allow_ssh_auth_sock":false}`)
	if p := loadFixturePolicy(t, f); !p.SSHAgent {
		t.Fatal("TTC environment policy disabled rail forwarding")
	}
	if allow, err := AllowSSHAuthSock(f.home, f.configHome); err != nil || allow {
		t.Fatalf("rail forwarding enabled TTC inheritance: allow=%v error=%v", allow, err)
	}
}

func TestSSHAgentHardlinksFailClosed(t *testing.T) {
	for _, source := range []string{"/socket-aliases/agent", "/socket-aliases", ""} {
		for _, enabled := range []bool{false, true} {
			t.Run(source+map[bool]string{false: " disabled", true: " enabled"}[enabled], func(t *testing.T) {
				f := newMountFixture(t)
				f.socket(t, "/tmp/agent/socket")
				f.env["SSH_AUTH_SOCK"] = "/tmp/agent/socket"
				f.dir(t, "/socket-aliases")
				if err := os.Link(f.host("/tmp/agent/socket"), f.host("/socket-aliases/agent")); err != nil {
					t.Fatal(err)
				}
				if source != "" {
					f.opts.Policy.Mounts = []Mount{{Source: source, Dest: "/alias"}}
				}
				f.opts.Policy.SSHAgent = enabled
				for _, denies := range [][]string{nil, {"/tmp/agent/socket"}} {
					f.opts.Policy.Denies = denies
					_, err := f.plan(context.Background())
					if err == nil || !strings.Contains(err.Error(), "SSH_AUTH_SOCK: multiple hardlinks") {
						t.Fatalf("hardlinked socket accepted: %v", err)
					}
				}
			})
		}
	}
}
