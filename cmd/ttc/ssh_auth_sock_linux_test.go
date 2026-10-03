package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHAuthSockPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		allow, fail  bool
	}{
		{name: "missing"},
		{name: "default", config: `{}`},
		{name: "false", config: `{"ttc_allow_ssh_auth_sock":false}`},
		{name: "true", config: `{"ttc_allow_ssh_auth_sock":true}`, allow: true},
		{name: "wrong type", config: `{"ttc_allow_ssh_auth_sock":"true"}`, fail: true},
		{name: "malformed", config: `{invalid`, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, config := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", config)
			t.Setenv("SSH_AUTH_SOCK", "/host/agent.sock")
			if tc.config != "" {
				if err := os.MkdirAll(filepath.Join(config, "ttc"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(config, "ttc", "rail.json"), []byte(tc.config), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := applySSHAuthSockPolicy()
			if (err != nil) != tc.fail {
				t.Fatalf("error = %v, want failure %v", err, tc.fail)
			}
			got, present := os.LookupEnv("SSH_AUTH_SOCK")
			if present != tc.allow || (present && got != "/host/agent.sock") {
				t.Fatalf("SSH_AUTH_SOCK = %q, present %v, want allowed %v", got, present, tc.allow)
			}
		})
	}
}

func TestSSHAuthSockToolEnvironment(t *testing.T) {
	if root := os.Getenv("TTC_TEST_SSH_ENV_ROOT"); root != "" {
		flag.CommandLine = flag.NewFlagSet("ttc", flag.ContinueOnError)
		os.Args = []string{"ttc", "--plain", "--offline-script", filepath.Join(root, "script.json"), "--workdir", root, "--data-dir", filepath.Join(root, "data")}
		if err := run(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("CLI requires ripgrep")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "opt-in"}[allow], func(t *testing.T) {
			root := t.TempDir()
			config := filepath.Join(root, "config")
			if err := os.MkdirAll(filepath.Join(config, "ttc"), 0700); err != nil {
				t.Fatal(err)
			}
			value := `{"ttc_allow_ssh_auth_sock":false}`
			want := "unset"
			if allow {
				value = `{"ttc_allow_ssh_auth_sock":true}`
				want = "/host/agent.sock"
			}
			if err := os.WriteFile(filepath.Join(config, "ttc", "rail.json"), []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
			// Project config cannot grant credentials to TTC outside rail either.
			if err := os.WriteFile(filepath.Join(root, "ttc-rail.json"), []byte(`{"ttc_allow_ssh_auth_sock":true}`), 0600); err != nil {
				t.Fatal(err)
			}
			script := `[{"calls":[{"id":"env-probe","name":"shell","arguments":{"command":"printf '%s' \"${SSH_AUTH_SOCK-unset}\" > ssh-env-result"}}]},{"text":"done"}]`
			if err := os.WriteFile(filepath.Join(root, "script.json"), []byte(script), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(executable, "-test.run=^TestSSHAuthSockToolEnvironment$")
			cmd.Env = append(os.Environ(), "TTC_TEST_SSH_ENV_ROOT="+root, "HOME="+root, "XDG_CONFIG_HOME="+config, "SSH_AUTH_SOCK=/host/agent.sock")
			cmd.Stdin = strings.NewReader("probe\n/quit\n")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("CLI: %v\n%s", err, output)
			}
			got, err := os.ReadFile(filepath.Join(root, "ssh-env-result"))
			if err != nil || string(got) != want {
				t.Fatalf("tool env = %q, error %v, want %q", got, err, want)
			}
		})
	}
}
