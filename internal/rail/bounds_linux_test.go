package rail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func policyEntries(prefix string, n int) []string {
	entries := make([]string, n)
	for i := range entries {
		entries[i] = fmt.Sprintf("/%s-%03d", prefix, i)
	}
	return entries
}

func writePolicyEntries(t *testing.T, path string, allow, deny []string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"allow": allow, "deny": deny})
	if err != nil {
		t.Fatal(err)
	}
	writePolicyConfig(t, path, string(data))
}

func TestPolicyFileEntryLimits(t *testing.T) {
	for _, tc := range []struct {
		allow, deny int
		invalid     bool
	}{
		{maxPolicyEntries, 0, false},
		{0, maxPolicyEntries, false},
		{128, 128, false},
		{maxPolicyEntries + 1, 0, true},
		{128, 129, true},
	} {
		t.Run(fmt.Sprintf("%d allow %d deny", tc.allow, tc.deny), func(t *testing.T) {
			f := newPolicyFixture(t)
			writePolicyEntries(t, f.global, policyEntries("allow", tc.allow), policyEntries("deny", tc.deny))
			_, err := LoadPolicy(f.home, f.configHome, f.workdir)
			if (err != nil) != tc.invalid {
				t.Fatalf("policy error=%v, want invalid=%v", err, tc.invalid)
			}
			if err != nil && !strings.Contains(err.Error(), "256 combined allow/deny entries") {
				t.Fatalf("unexpected limit error: %v", err)
			}
		})
	}
	// Duplicates and service shorthands count toward the file's input bound,
	// even though they collapse to fewer entries in the merged policy.
	f := newPolicyFixture(t)
	entries := make([]string, maxPolicyEntries+1)
	for i := range entries {
		entries[i] = "ssh-agent"
	}
	writePolicyEntries(t, f.global, []string{}, entries)
	if _, err := AllowSSHAuthSock(f.home, f.configHome); err == nil || !strings.Contains(err.Error(), "256 combined allow/deny entries") {
		t.Fatalf("runtime parser accepted too many service entries: %v", err)
	}
}

func TestMergedPolicyEntryLimits(t *testing.T) {
	f := newPolicyFixture(t)
	allow := policyEntries("allow", maxPolicyEntries)
	writePolicyEntries(t, f.global, allow, []string{})
	writePolicyEntries(t, f.project, allow, []string{})
	if policy := loadFixturePolicy(t, f); len(policy.Mounts) != maxPolicyEntries {
		t.Fatalf("exact destination replacements counted twice: %d", len(policy.Mounts))
	}
	// Enforce the merged bound before removing mounts covered by denies.
	writePolicyEntries(t, f.project, []string{}, allow[:1])
	if _, err := LoadPolicy(f.home, f.configHome, f.workdir); err == nil || !strings.Contains(err.Error(), "256 combined mount/deny entries") {
		t.Fatalf("oversized merged policy accepted: %v", err)
	}
	writePolicyEntries(t, f.global, []string{}, allow)
	writePolicyEntries(t, f.project, []string{}, allow)
	if policy := loadFixturePolicy(t, f); len(policy.Denies) != maxPolicyEntries {
		t.Fatalf("duplicate denies counted twice: %d", len(policy.Denies))
	}
}

func TestSandboxPolicyLimitsAndScale(t *testing.T) {
	for _, count := range []int{1, 64, maxPolicyEntries} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := newMountFixture(t)
			f.dir(t, "/source")
			for _, dest := range policyEntries("extra", count) {
				f.opts.Policy.Mounts = append(f.opts.Policy.Mounts, Mount{Source: "/source", Dest: dest})
			}
			plan := f.mustPlan(t)
			for _, mount := range f.opts.Policy.Mounts {
				requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host(mount.Source), dest: mount.Dest})
			}
			f.opts.Policy.Denies = []string{"/denied"}
			_, err := f.plan(context.Background())
			if (err != nil) != (count == maxPolicyEntries) {
				t.Fatalf("combined limit not enforced: %v", err)
			}
		})
	}
	for _, policy := range []Policy{
		{Mounts: make([]Mount, maxPolicyEntries+1)},
		{Denies: policyEntries("deny", maxPolicyEntries+1)},
		{Mounts: make([]Mount, 128), Denies: policyEntries("deny", 129)},
		{ConfigFiles: make([]string, maxPolicyConfigFiles+1)},
	} {
		// Limits are checked before inspecting even malformed paths or sources.
		if _, err := resolveSandboxSpec(context.Background(), sandboxOptions{Policy: policy}, sandboxEnvironment{}); err == nil || !strings.Contains(err.Error(), "rail policy exceeds") {
			t.Fatalf("direct policy escaped the bound: %v", err)
		}
	}
	f := newMountFixture(t)
	f.opts.Policy.Denies = policyEntries("deny", maxPolicyEntries)
	plan := f.mustPlan(t)
	for _, deny := range f.opts.Policy.Denies {
		requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemTmpfs, dest: deny})
	}
}

func TestSandboxPlanningCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolveSandboxSpec(ctx, sandboxOptions{}, sandboxEnvironment{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled context did not win over path validation: %v", err)
	}
	f := newMountFixture(t)
	spec, err := resolveSandboxSpec(context.Background(), f.opts, f.environment())
	if err != nil {
		t.Fatal(err)
	}
	_, err = planSandbox(ctx, spec, f.root)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("planning ignored cancellation between filesystem operations: %v", err)
	}
	p := mountPlanner{root: f.root, links: map[string]string{}}
	if err := p.placeOrderedMounts(ctx, []resolvedMount{{source: f.host("/usr"), dest: "/extra"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("ordered planner ignored cancellation: %v", err)
	}
	if len(p.plan.filesystem) != 0 || len(p.mounts) != 0 {
		t.Fatal("canceled planner appended a mount")
	}
}

func TestMountLinkResolutionCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	_, err := resolveMountLinks(ctx, "/first/second", func(string) (string, error) {
		reads++
		cancel()
		return "", nil
	})
	if !errors.Is(err, context.Canceled) || reads != 1 {
		t.Fatalf("link resolution continued after cancellation: reads=%d error=%v", reads, err)
	}
}
