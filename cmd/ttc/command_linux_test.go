package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/providers/builtin"
)

func TestCommandTreeHelp(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("TTC_DATA_DIR", "relative")
	for _, args := range [][]string{{"--help"}, {"help"}, {"help", "md"}, {"md", "--help"}, {"rail", "--help"}, {"watch-run", "--help"}} {
		root := newRootCommand(builtin.Modules())
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out.String(), "Usage:") {
			t.Fatalf("%v: missing help: %s", args, &out)
		}
		if len(args) == 1 && !strings.HasPrefix(out.String(), "TTC - True Terminal Coding\n") {
			t.Fatalf("%v: incorrect title: %s", args, &out)
		}
		if len(args) == 1 && strings.Contains(out.String(), "__rail") {
			t.Fatalf("private hooks exposed: %s", &out)
		}
	}
}

func TestCommandTreeRejectsBeforeSideEffects(t *testing.T) {
	rootDir := t.TempDir()
	data := filepath.Join(rootDir, "data")
	t.Setenv("SSH_AUTH_SOCK", "fixture-socket")
	for _, args := range [][]string{
		{"unexpected"}, {"--plain", "unexpected"}, {"--setup-exa", "unexpected"},
		{"--auto-name=false"}, {"--model", "x"}, {"--variant", "high"}, {"--web-search-config", "x"},
		{"--plain", "unexpected", "--unknown"}, {"md", "missing.md", "--unknown"},
		{"rail", "--list", "unexpected"}, {"__rail_serve", "unexpected"}, {"__rail_clients", "unexpected"},
	} {
		root := newRootCommand(builtin.Modules())
		root.SetArgs(append([]string{"--data-dir", data}, args...))
		if err := root.Execute(); err == nil {
			t.Fatalf("accepted %v", args)
		}
		if got := os.Getenv("SSH_AUTH_SOCK"); got != "fixture-socket" {
			t.Fatalf("%v changed socket: %q", args, got)
		}
		if _, err := os.Stat(data); !os.IsNotExist(err) {
			t.Fatalf("%v created data: %v", args, err)
		}
	}
}

func TestCommandTreesOwnIndependentFlags(t *testing.T) {
	first := newRootCommand(builtin.Modules())
	second := newRootCommand(builtin.Modules())
	if err := first.Flags().Set("plain", "true"); err != nil {
		t.Fatal(err)
	}
	if err := first.Flags().Set("openai-base-url", "https://fixture.invalid"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"plain", "openai-base-url"} {
		if second.Flags().Changed(name) || first.Flags().Lookup(name).Value == second.Flags().Lookup(name).Value {
			t.Fatalf("shared %s flag state", name)
		}
	}
}
