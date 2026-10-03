package rail

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxPlanHostRevalidation(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testing.T, *mountFixture)
	}{
		{"source content", func(t *testing.T, f *mountFixture) {
			if err := os.WriteFile(f.host(f.opts.Executable), []byte("changed executable content"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"source permissions", func(t *testing.T, f *mountFixture) {
			if err := os.Chmod(f.host(f.opts.Workdir), 0755); err != nil {
				t.Fatal(err)
			}
		}},
		{"system alias", func(t *testing.T, f *mountFixture) {
			if err := os.Remove(f.host("/bin")); err != nil {
				t.Fatal(err)
			}
			f.link(t, "/bin", "usr/lib")
		}},
		{"absent optional import", func(t *testing.T, f *mountFixture) {
			f.file(t, filepath.Join(f.opts.Home, ".bashrc"))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newMountFixture(t)
			plan := f.mustPlan(t)
			if len(plan.hostObservations) == 0 {
				t.Fatal("planner did not capture host observations")
			}
			if err := plan.validateHost(context.Background()); err != nil {
				t.Fatalf("unchanged host rejected: %v", err)
			}
			test.change(t, f)
			if err := plan.validateHost(context.Background()); err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("changed planning inputs accepted: %v", err)
			}
		})
	}
}

func TestSandboxPlanRevalidationAllowsGeneratedStartup(t *testing.T) {
	f := newMountFixture(t)
	plan := f.mustPlan(t)
	// Runtime writes the planned startup file into the imported private control
	// directory before launch. Directory child activity is not a tree snapshot.
	if err := os.WriteFile(f.host(filepath.Join(f.opts.SocketDir, "tmux.conf")), []byte(tmuxStartup(*plan)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := plan.validateHost(context.Background()); err != nil {
		t.Fatalf("generated startup invalidated its own plan: %v", err)
	}
}
