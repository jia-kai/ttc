package rail

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProtectedFileHardlinksFailClosed(t *testing.T) {
	for _, name := range []string{"mandatory project config", "global rail config", "web search config", "bashrc", "zshrc", "tmux config", "executable"} {
		for _, direct := range []bool{false, true} {
			t.Run(name+map[bool]string{false: " directory-hidden", true: " direct"}[direct], func(t *testing.T) {
				f := newMountFixture(t)
				path := map[string]string{
					"mandatory project config": filepath.Join(f.opts.Workdir, "ttc-rail.json"),
					"global rail config":       filepath.Join(f.opts.ConfigHome, "ttc", "rail.json"),
					"web search config":        filepath.Join(f.opts.ConfigHome, "ttc", "web-search.json"),
					"bashrc":                   filepath.Join(f.opts.Home, ".bashrc"),
					"zshrc":                    filepath.Join(f.opts.Home, ".zshrc"),
					"tmux config":              filepath.Join(f.opts.ConfigHome, "tmux", "tmux.conf"),
					"executable":               f.opts.Executable,
				}[name]
				f.file(t, path)
				if name == "mandatory project config" || name == "global rail config" {
					f.opts.Policy.ConfigFiles = []string{path}
				}
				alias := filepath.Join(f.opts.Workdir, "hidden", "writable-alias")
				f.dir(t, filepath.Dir(alias))
				if err := os.Link(f.host(path), f.host(alias)); err != nil {
					t.Fatal(err)
				}
				if direct {
					f.opts.Policy.Mounts = []Mount{{Source: alias, Dest: "/direct", Writable: true}}
				}
				_, err := f.plan(context.Background())
				if err == nil || !strings.Contains(err.Error(), "protected file") || !strings.Contains(err.Error(), "multiple hardlinks") {
					t.Fatalf("protected hardlinked file accepted: %v", err)
				}
			})
		}
	}
}

func TestPolicyFileHardlinksFailClosed(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(map[bool]string{false: "project", true: "global"}[global], func(t *testing.T) {
			f := newPolicyFixture(t)
			path := f.project
			if global {
				path = f.global
			}
			writePolicyConfig(t, path, `{}`)
			if err := os.Link(path, filepath.Join(f.workdir, "config-alias")); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadPolicy(f.home, f.configHome, f.workdir); err == nil || !strings.Contains(err.Error(), "multiple hardlinks") {
				t.Fatalf("hardlinked config parsed: %v", err)
			}
			if global {
				if _, err := AllowSSHAuthSock(f.home, f.configHome); err == nil || !strings.Contains(err.Error(), "multiple hardlinks") {
					t.Fatalf("TTC policy reader accepted hardlinked config: %v", err)
				}
			}
		})
	}
}

func TestDockerDefaultMountAuthorization(t *testing.T) {
	for _, location := range []string{"workdir", "data", "cache", "nvim", "ttc config", "socket dir", "system"} {
		t.Run(location, func(t *testing.T) {
			f := newMountFixture(t)
			parent := map[string]string{
				"workdir": f.opts.Workdir, "data": f.opts.DataDir, "cache": f.opts.CacheDir,
				"nvim": filepath.Join(f.opts.ConfigHome, "nvim"), "ttc config": filepath.Join(f.opts.ConfigHome, "ttc"),
				"socket dir": f.opts.SocketDir, "system": "/etc",
			}[location]
			endpoint := filepath.Join(parent, "docker.sock")
			f.socket(t, endpoint)
			f.link(t, "/run/docker.sock", endpoint)
			_, err := f.plan(context.Background())
			if err == nil || !strings.Contains(err.Error(), "globally authorized docker service") {
				t.Fatalf("default exposed Docker endpoint: %v", err)
			}
			f.opts.Policy.Docker = true
			plan := f.mustPlan(t)
			requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(endpoint), dest: "/run/docker.sock", writable: true})
		})
	}
}

func TestDockerHardlinksFailClosed(t *testing.T) {
	for _, source := range []string{"/socket-aliases/docker", "/socket-aliases", ""} {
		for _, enabled := range []bool{false, true} {
			t.Run(source+map[bool]string{false: " disabled", true: " enabled"}[enabled], func(t *testing.T) {
				f := newMountFixture(t)
				f.socket(t, "/run/docker.sock")
				f.dir(t, "/socket-aliases")
				if err := os.Link(f.host("/run/docker.sock"), f.host("/socket-aliases/docker")); err != nil {
					t.Fatal(err)
				}
				if source != "" {
					f.opts.Policy.Mounts = []Mount{{Source: source, Dest: "/extra"}}
				}
				f.opts.Policy.Docker = enabled
				_, err := f.plan(context.Background())
				if err == nil || !strings.Contains(err.Error(), "Docker endpoint: multiple hardlinks") {
					t.Fatalf("hardlinked Docker socket accepted: %v", err)
				}
			})
		}
	}
}

func TestOptionalImportsPreserveWritableRoots(t *testing.T) {
	for _, root := range []string{"workdir", "data", "cache"} {
		for _, config := range []string{"nvim", "zsh", "tmux", "ttc"} {
			for _, linked := range []bool{false, true} {
				t.Run(root+" "+config+map[bool]string{false: " exact", true: " symlink"}[linked], func(t *testing.T) {
					f := newMountFixture(t)
					optional := filepath.Join(f.opts.ConfigHome, config)
					var writable string
					if linked {
						writable = map[string]string{"workdir": f.opts.Workdir, "data": f.opts.DataDir, "cache": f.opts.CacheDir}[root]
						f.link(t, optional, writable)
					} else {
						writable = optional
						f.dir(t, writable)
						switch root {
						case "workdir":
							f.opts.Workdir = writable
						case "data":
							f.opts.DataDir = writable
						case "cache":
							f.opts.CacheDir = writable
						}
					}
					// The required directory remains writable, but its mandatory rail
					// policy entrypoint still receives a later read-only overlay.
					policyFile := filepath.Join(writable, "ttc-rail.json")
					f.file(t, policyFile)
					f.opts.Policy.ConfigFiles = []string{policyFile}
					plan := f.mustPlan(t)
					bind := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(writable), dest: writable, writable: true})
					aliasBind := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(writable), dest: optional, writable: true})
					readonly := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(policyFile), dest: policyFile})
					if bind >= readonly {
						t.Fatal("mandatory config overlay precedes writable root")
					}
					aliasPolicy := filepath.Join(optional, "ttc-rail.json")
					if requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(policyFile), dest: aliasPolicy}) <= aliasBind {
						t.Fatal("mandatory config overlay precedes forwarded writable alias")
					}
					for _, operation := range plan.filesystem {
						if operation.kind == filesystemBind && !operation.writable && operation.source == f.host(writable) {
							t.Fatalf("optional import made required root or an alias read-only: %+v", operation)
						}
					}
				})
			}
		}
	}
}
