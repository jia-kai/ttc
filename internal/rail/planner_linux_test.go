package rail

import (
	"context"
	"strings"
	"testing"
)

func TestPrimaryMountCanonicalDependencyCycle(t *testing.T) {
	f := newMountFixture(t)
	f.dir(t, "/sources/parent")
	f.link(t, "/sources/parent/child", "/target")
	f.dir(t, "/sources/child")
	p := mountPlanner{root: f.root, links: map[string]string{"/a": "/target/inner"}}
	err := p.placeOrderedMounts(context.Background(), []resolvedMount{
		{source: f.host("/sources/parent"), dest: "/a"},
		{source: f.host("/sources/child"), dest: "/a/child"},
	})
	if err == nil || !strings.Contains(err.Error(), "cyclic") {
		t.Fatalf("canonical ancestry cycle must fail loudly: %v", err)
	}
}

func TestPrimaryMountSeveralCanonicalAncestorReplays(t *testing.T) {
	f := newMountFixture(t)
	f.file(t, "/sources/child")
	f.dir(t, "/sources/parent/deep")
	f.dir(t, "/sources/z")
	f.link(t, "/sources/z/link", "/a/deep")
	f.dir(t, "/sources/last")
	f.dir(t, "/sources/zz")
	f.link(t, "/sources/zz/link", "/a")
	f.opts.Policy.Mounts = []Mount{
		{Source: "/sources/child", Dest: "/a/deep/leaf"},
		{Source: "/sources/z", Dest: "/z"},
		{Source: "/sources/parent", Dest: "/z/link", Writable: true},
		{Source: "/sources/zz", Dest: "/zz"},
		{Source: "/sources/last", Dest: "/zz/link", Writable: true},
	}
	plan := f.mustPlan(t)
	outer := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/sources/last"), dest: "/a", writable: true})
	inner := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/sources/parent"), dest: "/a/deep", writable: true})
	child := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemBind, source: f.host("/sources/child"), dest: "/a/deep/leaf"})
	if outer >= inner || inner >= child {
		t.Fatalf("bad ancestor order after multiple replays: outer=%d inner=%d child=%d", outer, inner, child)
	}
	for _, operation := range plan.filesystem[child+1:] {
		if operation.kind == filesystemBind && mountContains(operation.dest, "/a/deep/leaf") {
			t.Fatalf("later bind hides the final child: %+v", operation)
		}
	}
}

func TestPrimaryMountUnhiddenSymlinkLoop(t *testing.T) {
	f := newMountFixture(t)
	f.dir(t, "/sources/tree")
	f.link(t, "/sources/tree/bad", "bad")
	f.file(t, "/sources/leaf")
	f.opts.Policy.Mounts = []Mount{
		{Source: "/sources/tree", Dest: "/tree"},
		{Source: "/sources/leaf", Dest: "/tree/bad/leaf"},
	}
	if plan, err := f.plan(context.Background()); err == nil || plan != nil || !strings.Contains(err.Error(), "too many symlinks") {
		t.Fatalf("unhidden loop must fail loudly: plan=%v err=%v", plan, err)
	}
}
