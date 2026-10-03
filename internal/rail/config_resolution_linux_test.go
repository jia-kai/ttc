//go:build linux

package rail

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolvePolicyIndependentOfFilesAndEnvironment(t *testing.T) {
	f := newPolicyFixture(t)
	writePolicyConfig(t, f.global, `{
		"authorize_services":["docker","ssh-agent"],
		"ttc_allow_ssh_auth_sock":true,
		"allow":[{"source":"old","dest":"/replace","mode":"rw"},
			{"source":"~/assets","dest":"/keep"},
			{"source":"hidden","dest":"/secret/child"},
			{"source":"prefix","dest":"/secretary"}],
		"deny":["/secret","ssh-agent"]
	}`)
	writePolicyConfig(t, f.project, `{
		"allow":[{"source":"$HOME/new","dest":"/replace"},
			{"source":"child","dest":"/keep/child","mode":"rw"},
			"docker","ssh-agent"],
		"deny":["/secret",{"dest":"/keep/private"}]
	}`)
	global, err := loadPolicyFile(f.global, f.home, true)
	if err != nil {
		t.Fatal(err)
	}
	project, err := loadPolicyFile(f.project, f.home, false)
	if err != nil {
		t.Fatal(err)
	}
	// Neither the config entrypoints nor the explicitly allowed sources remain
	// available. Resolution must use only the already parsed, lexical paths.
	if err := os.RemoveAll(filepath.Dir(f.home)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "SSH_AUTH_SOCK", "DOCKER_HOST"} {
		t.Setenv(name, "invalid-relative-value")
	}
	want := Policy{
		Mounts: []Mount{
			{Source: filepath.Join(f.workdir, "$HOME/new"), Dest: "/replace"},
			{Source: filepath.Join(f.home, "assets"), Dest: "/keep"},
			{Source: filepath.Join(filepath.Dir(f.global), "prefix"), Dest: "/secretary"},
			{Source: filepath.Join(f.workdir, "child"), Dest: "/keep/child", Writable: true},
		},
		Denies:      []string{"/secret", "/keep/private"},
		ConfigFiles: []string{f.global, f.project},
		Docker:      true,
	}
	got, err := resolvePolicy(global, project)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved = %#v, want %#v", got, want)
	}
	// Mutating returned slices must not mutate either input layer or future
	// resolutions, including entries replaced and filtered during this merge.
	got.Mounts[0].Source = "/changed"
	got.Denies[0] = "/changed"
	got.ConfigFiles[0] = "/changed"
	again, err := resolvePolicy(global, project)
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("resolution mutated input layers: %#v, %v", again, err)
	}
}

func TestResolvePolicyServices(t *testing.T) {
	for _, service := range []string{"docker", "ssh-agent"} {
		for _, tc := range []struct {
			name, global, project string
			enabled, invalid      bool
		}{
			{"authorization alone", `{"authorize_services":[%q]}`, `{}`, false, false},
			{"global request", `{"authorize_services":[%q],"allow":[%q]}`, `{}`, true, false},
			{"project request", `{"authorize_services":[%q]}`, `{"allow":[%q]}`, true, false},
			{"project deny", `{"authorize_services":[%q],"allow":[%q]}`, `{"deny":[%q]}`, false, false},
			{"global deny", `{"authorize_services":[%q],"deny":[%q]}`, `{"allow":[%q]}`, false, false},
			{"unauthorized request", `{}`, `{"allow":[%q]}`, false, true},
			{"denied unauthorized request", `{"deny":[%q]}`, `{"allow":[%q]}`, false, true},
			{"deny alone", `{}`, `{"deny":[%q]}`, false, false},
		} {
			t.Run(service+"/"+tc.name, func(t *testing.T) {
				parse := func(template string, global bool) filePolicy {
					t.Helper()
					config := strings.ReplaceAll(template, "%q", fmt.Sprintf("%q", service))
					parsed, err := parsePolicy([]byte(config), "/config", "/home/user", global)
					if err != nil {
						t.Fatal(err)
					}
					return parsed
				}
				got, err := resolvePolicy(parse(tc.global, true), parse(tc.project, false))
				if (err != nil) != tc.invalid {
					t.Fatalf("error = %v, want invalid = %v", err, tc.invalid)
				}
				if tc.invalid {
					if !strings.Contains(err.Error(), service+" requires authorize_services") {
						t.Fatalf("unexpected authorization error: %v", err)
					}
					return
				}
				enabled := got.Docker
				if service == "ssh-agent" {
					enabled = got.SSHAgent
				}
				if enabled != tc.enabled || len(got.Mounts) != 0 {
					t.Fatalf("resolved service policy = %#v, want enabled = %v without socket mounts", got, tc.enabled)
				}
			})
		}
	}
}

func TestResolvePolicyLimitsBeforeDenyFiltering(t *testing.T) {
	parse := func(allow, deny []string) filePolicy {
		t.Helper()
		data, err := json.Marshal(map[string]any{"allow": allow, "deny": deny})
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parsePolicy(data, "/config", "/home/user", true)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	paths := make([]string, maxPolicyEntries)
	for i := range paths {
		paths[i] = fmt.Sprintf("/entry-%03d", i)
	}
	global := parse(paths, []string{})
	project := parse(paths, []string{})
	if got, err := resolvePolicy(global, project); err != nil || len(got.Mounts) != maxPolicyEntries {
		t.Fatalf("destination replacements counted twice: %#v, %v", got, err)
	}
	project = parse([]string{}, paths[:1])
	if got, err := resolvePolicy(global, project); err == nil || !strings.Contains(err.Error(), "256 combined mount/deny entries") {
		t.Fatalf("deny filtering hid oversized policy: %#v, %v", got, err)
	}
	global = parse(paths[:maxPolicyEntries-1], []string{})
	if got, err := resolvePolicy(global, project); err != nil || len(got.Mounts) != maxPolicyEntries-2 {
		t.Fatalf("policy at limit before deny filtering: %#v, %v", got, err)
	}
	global = parse([]string{}, paths)
	project = parse([]string{}, paths)
	if got, err := resolvePolicy(global, project); err != nil || len(got.Denies) != maxPolicyEntries {
		t.Fatalf("duplicate denies counted twice: %#v, %v", got, err)
	}
}

func TestResolvePolicyMissingAndSharedConfigEntrypoints(t *testing.T) {
	if got, err := resolvePolicy(filePolicy{}, filePolicy{}); err != nil || !reflect.DeepEqual(got, Policy{}) {
		t.Fatalf("empty layers = %#v, %v", got, err)
	}
	layer := filePolicy{configFile: "/absent/rail.json"}
	got, err := resolvePolicy(layer, layer)
	if err != nil || !reflect.DeepEqual(got.ConfigFiles, []string{layer.configFile}) {
		t.Fatalf("shared config entrypoints = %#v, %v", got, err)
	}
}
