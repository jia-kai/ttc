//go:build linux

package rail

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

type policyFixture struct {
	home, configHome, workdir string
	global, project           string
}

func newPolicyFixture(t *testing.T) policyFixture {
	t.Helper()
	root := t.TempDir()
	f := policyFixture{
		home: filepath.Join(root, "home"), configHome: filepath.Join(root, "config"), workdir: filepath.Join(root, "project"),
	}
	f.global = filepath.Join(f.configHome, "ttc", "rail.json")
	f.project = filepath.Join(f.workdir, "ttc-rail.json")
	for _, dir := range []string{f.home, filepath.Dir(f.global), f.workdir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func writePolicyConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func loadFixturePolicy(t *testing.T, f policyFixture) Policy {
	t.Helper()
	p, err := LoadPolicy(f.home, f.configHome, f.workdir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadPolicyMissingAndDefaults(t *testing.T) {
	f := newPolicyFixture(t)
	if p := loadFixturePolicy(t, f); !reflect.DeepEqual(p, Policy{}) {
		t.Fatalf("missing configs: %#v", p)
	}
	writePolicyConfig(t, f.global, `{"allow":["../shared",{"source":"~/assets"},{"source":"relative","dest":"../target","mode":"rw"},"$HOME/literal"]}`)
	p := loadFixturePolicy(t, f)
	base := filepath.Dir(f.global)
	want := []Mount{
		{Source: filepath.Join(f.configHome, "shared"), Dest: filepath.Join(f.configHome, "shared")},
		{Source: filepath.Join(f.home, "assets"), Dest: filepath.Join(f.home, "assets")},
		{Source: filepath.Join(base, "relative"), Dest: filepath.Join(f.configHome, "target"), Writable: true},
		{Source: filepath.Join(base, "$HOME/literal"), Dest: filepath.Join(base, "$HOME/literal")},
	}
	if !reflect.DeepEqual(p.Mounts, want) {
		t.Fatalf("mounts = %#v, want %#v", p.Mounts, want)
	}
	if !reflect.DeepEqual(p.ConfigFiles, []string{f.global}) {
		t.Fatalf("config files = %v", p.ConfigFiles)
	}
	// Explicit allow sources do not need to exist until mount planning.
	if _, err := os.Stat(p.Mounts[0].Source); !os.IsNotExist(err) {
		t.Fatalf("fixture unexpectedly exists: %v", err)
	}
}

func TestLoadPolicyMergeDeniesWin(t *testing.T) {
	f := newPolicyFixture(t)
	writePolicyConfig(t, f.global, `{"allow":[{"source":"old","dest":"/replace","mode":"rw"},{"source":"keep","dest":"/keep"},{"source":"blocked","dest":"/secret/child"},{"source":"ancestor","dest":"/parent"},{"source":"prefix","dest":"/secretary"}],"deny":["/secret","/parent/hidden"]}`)
	writePolicyConfig(t, f.project, `{"allow":[{"source":"new","dest":"/replace"},{"source":"bad","dest":"/secret/new"},{"source":"local","dest":"local-dest"}],"deny":["/secret",{"source":"does-not-exist","dest":"/keep"},{"dest":"/other"}]}`)
	p := loadFixturePolicy(t, f)
	want := []Mount{
		{Source: filepath.Join(f.workdir, "new"), Dest: "/replace"},
		{Source: filepath.Join(filepath.Dir(f.global), "ancestor"), Dest: "/parent"},
		{Source: filepath.Join(filepath.Dir(f.global), "prefix"), Dest: "/secretary"},
		{Source: filepath.Join(f.workdir, "local"), Dest: filepath.Join(f.workdir, "local-dest")},
	}
	if !reflect.DeepEqual(p.Mounts, want) {
		t.Fatalf("mounts = %#v, want %#v", p.Mounts, want)
	}
	if want := []string{"/secret", "/parent/hidden", "/keep", "/other"}; !reflect.DeepEqual(p.Denies, want) {
		t.Fatalf("denies = %v, want %v", p.Denies, want)
	}
	if want := []string{f.global, f.project}; !reflect.DeepEqual(p.ConfigFiles, want) {
		t.Fatalf("config files = %v, want %v", p.ConfigFiles, want)
	}
}

func TestLoadPolicyDenyRoot(t *testing.T) {
	f := newPolicyFixture(t)
	writePolicyConfig(t, f.global, `{"allow":["/","/child"],"deny":["/"]}`)
	if p := loadFixturePolicy(t, f); len(p.Mounts) != 0 || !reflect.DeepEqual(p.Denies, []string{"/"}) {
		t.Fatalf("root deny: %#v", p)
	}
}

func TestLoadPolicyDocker(t *testing.T) {
	tests := []struct {
		name, global, project string
		wantDocker, wantError bool
	}{
		{"authorized only", `{"authorize_services":["docker"]}`, `{}`, false, false},
		{"global request", `{"authorize_services":["docker"],"allow":["docker"]}`, `{}`, true, false},
		{"project request", `{"authorize_services":["docker"]}`, `{"allow":["docker"]}`, true, false},
		{"unauthorized", `{}`, `{"allow":["docker"]}`, false, true},
		{"global unauthorized", `{"allow":["docker"]}`, `{}`, false, true},
		{"project deny", `{"authorize_services":["docker"],"allow":["docker"]}`, `{"deny":["docker"]}`, false, false},
		{"global deny", `{"authorize_services":["docker"],"deny":["docker"]}`, `{"allow":["docker"]}`, false, false},
		{"project authorization", `{}`, `{"authorize_services":["docker"]}`, false, true},
		{"empty project authorization", `{}`, `{"authorize_services":[]}`, false, true},
		{"null project authorization", `{}`, `{"authorize_services":null}`, false, true},
		{"unknown service", `{"authorize_services":["podman"]}`, `{}`, false, true},
		{"deny only", `{}`, `{"deny":["docker"]}`, false, false},
		{"denied unauthorized request", `{"deny":["docker"]}`, `{"allow":["docker"]}`, false, true},
		{"ordinary docker path", `{}`, `{"allow":[{"source":"docker"}]}`, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPolicyFixture(t)
			writePolicyConfig(t, f.global, tt.global)
			writePolicyConfig(t, f.project, tt.project)
			p, err := LoadPolicy(f.home, f.configHome, f.workdir)
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v, wantError = %v", err, tt.wantError)
			}
			if p.Docker != tt.wantDocker {
				t.Fatalf("docker = %v, want %v", p.Docker, tt.wantDocker)
			}
			if p.Docker && len(p.Mounts) != 0 {
				t.Fatalf("docker must not add a socket mount during parsing: %#v", p.Mounts)
			}
		})
	}
}

func TestLoadPolicyStrictJSON(t *testing.T) {
	invalid := []string{
		``, `null`, `[]`, `42`, `{"allow":`, `{} {}`, `{} true`, `{} garbage`,
		`{"unknown":[]}`, `{"Allow":[]}`, `{"allow":[],"allow":[]}`,
		`{"allow":null}`, `{"allow":"/path"}`, `{"deny":{}}`, `{"authorize_services":null}`,
		`{"allow":[null]}`, `{"allow":[1]}`, `{"allow":[true]}`, `{"allow":[[]]}`,
		`{"allow":[""]}`, `{"allow":["~other/path"]}`, `{"allow":["/nul\u0000"]}`,
		`{"allow":[{}]}`, `{"allow":[{"dest":"/target"}]}`, `{"allow":[{"source":null}]}`,
		`{"allow":[{"source":"x","dest":""}]}`, `{"allow":[{"source":"x","mode":""}]}`,
		`{"allow":[{"source":"x","mode":"write"}]}`, `{"allow":[{"source":"x","mode":true}]}`,
		`{"allow":[{"source":"x","unknown":true}]}`, `{"allow":[{"source":"x","source":"y"}]}`,
		`{"deny":[{}]}`, `{"deny":[{"source":"x","mode":"ro"}]}`, `{"deny":[{"dest":false}]}`,
		`{"authorize_services":[null]}`, `{"authorize_services":[{"source":"docker"}]}`,
	}
	for _, config := range invalid {
		t.Run(config, func(t *testing.T) {
			f := newPolicyFixture(t)
			writePolicyConfig(t, f.global, config)
			p, err := LoadPolicy(f.home, f.configHome, f.workdir)
			if err == nil {
				t.Fatalf("accepted %q: %#v", config, p)
			}
			if !strings.Contains(err.Error(), f.global) {
				t.Fatalf("missing config error context: %v", err)
			}
		})
	}
}

func TestLoadPolicyBoundedAndNonregular(t *testing.T) {
	f := newPolicyFixture(t)
	writePolicyConfig(t, f.global, `{}`+strings.Repeat(" ", maxConfigBytes-2))
	loadFixturePolicy(t, f)
	writePolicyConfig(t, f.global, `{}`+strings.Repeat(" ", maxConfigBytes-1))
	if _, err := LoadPolicy(f.home, f.configHome, f.workdir); err == nil {
		t.Fatal("accepted oversized config")
	}
	if err := os.Remove(f.global); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.global, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(f.home, f.configHome, f.workdir); err == nil {
		t.Fatal("accepted directory as config")
	}
}

func TestLoadPolicyRejectsSymlinkAndDanglingSymlink(t *testing.T) {
	f := newPolicyFixture(t)
	realPath := filepath.Join(t.TempDir(), "actual.json")
	writePolicyConfig(t, realPath, `{"allow":["relative"]}`)
	if err := os.Symlink(realPath, f.project); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(f.home, f.configHome, f.workdir); err == nil {
		t.Fatal("accepted replaceable symlink config entrypoint")
	}
	if err := os.Remove(realPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(f.home, f.configHome, f.workdir); err == nil {
		t.Fatal("silently skipped dangling config symlink")
	}
}

func TestLoadPolicyRejectsFIFO(t *testing.T) {
	f := newPolicyFixture(t)
	if err := syscall.Mkfifo(f.global, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(f.home, f.configHome, f.workdir); err == nil {
		t.Fatal("accepted FIFO as config")
	}
}

func TestLoadPolicyRootsAndFallback(t *testing.T) {
	f := newPolicyFixture(t)
	for _, roots := range [][3]string{
		{"", f.configHome, f.workdir}, {"relative", f.configHome, f.workdir},
		{f.home, "relative", f.workdir}, {f.home, f.configHome, "relative"},
		{f.home + "\x00", f.configHome, f.workdir}, {f.home, f.configHome + "\x00", f.workdir},
		{f.home, f.configHome, f.workdir + "\x00"},
	} {
		if _, err := LoadPolicy(roots[0], roots[1], roots[2]); err == nil {
			t.Fatalf("accepted unsafe roots: %q", roots)
		}
	}
	fallback := filepath.Join(f.home, ".config", "ttc", "rail.json")
	if err := os.MkdirAll(filepath.Dir(fallback), 0700); err != nil {
		t.Fatal(err)
	}
	contents, err := json.Marshal(map[string]any{"allow": []string{"~"}})
	if err != nil {
		t.Fatal(err)
	}
	writePolicyConfig(t, fallback, string(contents))
	p, err := LoadPolicy(f.home, "", f.workdir)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Mounts) != 1 || p.Mounts[0].Source != f.home || !reflect.DeepEqual(p.ConfigFiles, []string{fallback}) {
		t.Fatalf("fallback policy = %#v", p)
	}
}
