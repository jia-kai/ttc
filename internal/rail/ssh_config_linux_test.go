package rail

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func planSSHFixture(ctx context.Context, f *mountFixture) (*sandboxPlan, error) {
	env := f.environment()
	env.UID = os.Geteuid()
	spec, err := resolveSandboxSpec(ctx, f.opts, env)
	if err != nil {
		return nil, err
	}
	return planSandbox(ctx, spec, f.root)
}

func mustSSHPlan(t *testing.T, f *mountFixture) *sandboxPlan {
	t.Helper()
	plan, err := planSSHFixture(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func sshSnapshots(plan *sandboxPlan) []filesystemOperation {
	var files []filesystemOperation
	for _, op := range plan.filesystem {
		if op.kind == filesystemFile {
			files = append(files, op)
		}
	}
	return files
}

func TestSSHConfigSnapshotSymlinksAndOwnedBytes(t *testing.T) {
	f := newMountFixture(t)
	target := "/usr/lib/vendor/ssh.conf"
	f.file(t, target)
	data := "Host *\n Port 2222\n# Exact bytes: \x00\xff\n"
	if err := os.WriteFile(f.host(target), []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"10-vendor.conf", "20-alias.conf"} {
		f.link(t, filepath.Join(sshConfigDirectory, name), target)
	}
	plan := mustSSHPlan(t, f)
	files := sshSnapshots(plan)
	if len(files) != 1 || files[0] != (filesystemOperation{kind: filesystemFile, dest: target, data: data}) {
		t.Fatalf("canonical snapshot = %#v", files)
	}
	if len(plan.sshDirectories) != 1 {
		t.Fatal("drop-in directory membership was not observed")
	}
	if err := plan.validateHost(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.host(target), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	if files[0].data != data {
		t.Fatal("planned snapshot followed live source changes")
	}
	if err := plan.validateHost(context.Background()); err == nil {
		t.Fatal("source changes accepted before launch")
	}
}

func TestSSHConfigEffectiveImportsAndSyntheticEntries(t *testing.T) {
	for _, kind := range []string{"directory", "file", "symlinked directory"} {
		t.Run(kind, func(t *testing.T) {
			f := newMountFixture(t)
			f.file(t, "/sources/custom.conf")
			var want string
			switch kind {
			case "directory":
				f.file(t, filepath.Join(sshConfigDirectory, "host.conf"))
				f.dir(t, "/sources/dropins")
				f.link(t, "/sources/dropins/custom.conf", "/custom/custom.conf")
				f.opts.Policy.Mounts = []Mount{{Source: "/sources/dropins", Dest: sshConfigDirectory}, {Source: "/sources/custom.conf", Dest: "/custom/custom.conf"}}
				want = "/custom/custom.conf"
			case "file":
				// The underlying directory is absent; a file bind creates it.
				want = filepath.Join(sshConfigDirectory, "custom.conf")
				f.opts.Policy.Mounts = []Mount{{Source: "/sources/custom.conf", Dest: want}}
			case "symlinked directory":
				f.dir(t, "/usr/share/dropins")
				f.link(t, sshConfigDirectory, "/usr/share/dropins")
				f.opts.Policy.Mounts = []Mount{{Source: "/sources/custom.conf", Dest: filepath.Join(sshConfigDirectory, "custom.conf")}}
				want = "/usr/share/dropins/custom.conf"
			}
			plan := mustSSHPlan(t, f)
			files := sshSnapshots(plan)
			if len(files) != 1 || files[0].dest != want || files[0].data != "fixture" {
				t.Fatalf("effective import snapshots = %#v, want %s", files, want)
			}
		})
	}
}

func TestSSHConfigDeniedUnsafeSourcesAndMasksLast(t *testing.T) {
	for _, deny := range []string{sshConfigDirectory, "/etc/ssh", filepath.Join(sshConfigDirectory, "unsafe.conf"), "/usr/lib/vendor"} {
		t.Run(deny, func(t *testing.T) {
			f := newMountFixture(t)
			f.file(t, "/usr/lib/vendor/unsafe.conf")
			if err := os.Chmod(f.host("/usr/lib/vendor/unsafe.conf"), 0666); err != nil {
				t.Fatal(err)
			}
			f.link(t, filepath.Join(sshConfigDirectory, "unsafe.conf"), "/usr/lib/vendor/unsafe.conf")
			f.opts.Policy.Denies = []string{deny}
			for _, file := range sshSnapshots(mustSSHPlan(t, f)) {
				if file.data != "" {
					t.Fatal("denied unsafe contents were snapshotted", file)
				}
			}
		})
	}
	f := newMountFixture(t)
	f.file(t, filepath.Join(sshConfigDirectory, "good.conf"))
	f.opts.Policy.Denies = []string{"/unrelated"}
	plan := mustSSHPlan(t, f)
	copy := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemFile, dest: filepath.Join(sshConfigDirectory, "good.conf"), data: "fixture"})
	mask := requireFilesystemOperation(t, plan, filesystemOperation{kind: filesystemTmpfs, dest: "/unrelated"})
	if copy >= mask {
		t.Fatal("generated config overrode a final deny mask")
	}
}

func TestSSHConfigHiddenEntriesAndReservedTargets(t *testing.T) {
	f := newMountFixture(t)
	path := filepath.Join(sshConfigDirectory, ".hidden.conf")
	f.file(t, path)
	if err := os.Chmod(f.host(path), 0666); err != nil {
		t.Fatal(err)
	}
	f.file(t, "/sources/hidden")
	f.opts.Policy.Mounts = []Mount{{Source: "/sources/hidden", Dest: filepath.Join(sshConfigDirectory, ".imported.conf")}}
	if files := sshSnapshots(mustSSHPlan(t, f)); len(files) != 0 {
		t.Fatal("OpenSSH's glob does not select hidden entries", files)
	}
	for _, target := range []string{"/proc/config", "/dev/null", "/run/ttc-rail/config"} {
		t.Run(target, func(t *testing.T) {
			f := newMountFixture(t)
			f.link(t, filepath.Join(sshConfigDirectory, "escape.conf"), target)
			if _, err := planSSHFixture(context.Background(), f); err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("reserved target accepted: %v", err)
			}
		})
	}
}

func TestSSHConfigRejectsUnsafeModesAndTypes(t *testing.T) {
	for _, kind := range []string{"group writable", "other writable", "directory", "socket", "missing target"} {
		t.Run(kind, func(t *testing.T) {
			f := newMountFixture(t)
			path := filepath.Join(sshConfigDirectory, "unsafe.conf")
			switch kind {
			case "group writable", "other writable":
				f.file(t, path)
				mode := fs.FileMode(0664)
				if kind == "other writable" {
					mode = 0646
				}
				if err := os.Chmod(f.host(path), mode); err != nil {
					t.Fatal(err)
				}
			case "directory":
				f.dir(t, path)
			case "socket":
				f.socket(t, path)
			case "missing target":
				f.link(t, path, "/missing/config")
			}
			if _, err := planSSHFixture(context.Background(), f); err == nil {
				t.Fatal("unsafe config accepted")
			}
		})
	}
}

type sshOwnerInfo struct {
	fs.FileInfo
	stat syscall.Stat_t
}

func (v sshOwnerInfo) Sys() any { return &v.stat }

func TestSSHConfigUntrustedOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := *info.Sys().(*syscall.Stat_t)
	stat.Uid = 65534
	var h hostInspection
	if _, err := h.readSSHConfig(context.Background(), path, sshOwnerInfo{FileInfo: info, stat: stat}, 1234); err == nil || !strings.Contains(err.Error(), "owner 65534") {
		t.Fatalf("untrusted overflow owner accepted: %v", err)
	}
}

func TestSSHConfigLimits(t *testing.T) {
	for _, kind := range []string{"entry count", "file bytes", "total bytes"} {
		t.Run(kind, func(t *testing.T) {
			f := newMountFixture(t)
			switch kind {
			case "entry count":
				for i := range maxSSHConfigEntries + 1 {
					f.file(t, filepath.Join(sshConfigDirectory, fmt.Sprintf("%03d.conf", i)))
				}
			case "file bytes":
				path := filepath.Join(sshConfigDirectory, "large.conf")
				f.file(t, path)
				if err := os.Truncate(f.host(path), maxSSHConfigBytes+1); err != nil {
					t.Fatal(err)
				}
			case "total bytes":
				for i := range maxSSHConfigTotalBytes/maxSSHConfigBytes + 1 {
					path := filepath.Join(sshConfigDirectory, fmt.Sprintf("%d.conf", i))
					f.file(t, path)
					if err := os.Truncate(f.host(path), maxSSHConfigBytes); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := planSSHFixture(context.Background(), f); err == nil || !strings.Contains(err.Error(), "exceed") {
				t.Fatalf("limit not enforced: %v", err)
			}
		})
	}
}

func TestSSHConfigDirectoryAndSymlinkRevalidation(t *testing.T) {
	for _, kind := range []string{"new entry", "removed entry", "retargeted symlink", "replaced directory"} {
		t.Run(kind, func(t *testing.T) {
			f := newMountFixture(t)
			f.file(t, "/usr/lib/vendor/one.conf")
			f.file(t, "/usr/lib/vendor/two.conf")
			link := filepath.Join(sshConfigDirectory, "vendor.conf")
			f.link(t, link, "/usr/lib/vendor/one.conf")
			plan := mustSSHPlan(t, f)
			switch kind {
			case "new entry":
				f.file(t, filepath.Join(sshConfigDirectory, "new.conf"))
			case "removed entry", "retargeted symlink":
				if err := os.Remove(f.host(link)); err != nil {
					t.Fatal(err)
				}
				if kind == "retargeted symlink" {
					f.link(t, link, "/usr/lib/vendor/two.conf")
				}
			case "replaced directory":
				if err := os.Rename(f.host(sshConfigDirectory), f.host(sshConfigDirectory)+".old"); err != nil {
					t.Fatal(err)
				}
				f.dir(t, sshConfigDirectory)
			}
			if err := plan.validateHost(context.Background()); err == nil {
				t.Fatal("changed selection accepted before launch")
			}
		})
	}
}

func TestSSHConfigReadRejectsSubstitutionAndCancellation(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"regular", "symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			var h hostInspection
			info, err := h.stat(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "regular":
				err = os.WriteFile(path, []byte("replaced"), 0600)
			case "symlink":
				err = os.Symlink(path+".old", path)
			case "fifo":
				err = unix.Mkfifo(path, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				_, err := h.readSSHConfig(ctx, path, info, os.Geteuid())
				result <- err
			}()
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("substitution read with old trust metadata")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("substituted file blocked snapshot reading")
			}
		})
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := planSSHFixture(cancelled, newMountFixture(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled planning = %v", err)
	}
}

func TestSSHConfigSnapshotsInBubblewrap(t *testing.T) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("Bubblewrap is optional outside rail runtime")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH client is optional outside this regression")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, bwrap, "--unshare-user", "--unshare-pid", "--ro-bind", "/", "/", "--", "/bin/true").CombinedOutput(); err != nil {
		t.Skipf("Bubblewrap namespaces unavailable: %v: %s", err, out)
	}
	root := t.TempDir()
	opts := sandboxOptions{Workdir: filepath.Join(root, "work"), Home: filepath.Join(root, "home"), DataDir: filepath.Join(root, "data"), CacheDir: filepath.Join(root, "cache"), ConfigHome: filepath.Join(root, "home/.config"), SocketDir: filepath.Join(root, "control"), Hostname: "fixture"}
	opts.Executable, err = os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ssh, vendor := filepath.Join(root, "ssh"), filepath.Join(root, "vendor")
	for _, path := range []string{opts.Workdir, opts.Home, opts.DataDir, opts.CacheDir, opts.SocketDir, filepath.Join(ssh, "ssh_config.d"), vendor} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ssh, "ssh_config"), []byte("Include /etc/ssh/ssh_config.d/*.conf\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vendor, "vendor.conf"), []byte("Host *\n Port 2222\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/vendor/vendor.conf", filepath.Join(ssh, "ssh_config.d/vendor.conf")); err != nil {
		t.Fatal(err)
	}
	opts.Policy.Mounts = []Mount{{Source: ssh, Dest: "/etc/ssh"}, {Source: vendor, Dest: "/vendor"}, {Source: vendor, Dest: "/original", Writable: true}}
	spec, err := resolveSandboxSpec(ctx, opts, sandboxEnvironment{Path: "/usr/bin:/bin", UID: os.Geteuid()})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planSandbox(ctx, spec, "/")
	if err != nil {
		t.Fatal(err)
	}
	for _, denied := range []bool{false, true} {
		t.Run(fmt.Sprint(denied), func(t *testing.T) {
			selected := plan
			script := `test -L /etc/ssh/ssh_config.d/vendor.conf && test "$(stat -c %u /vendor/vendor.conf)" = "$(id -u)" && ! (printf BAD > /vendor/vendor.conf) && printf 'Host *\n Port 3333\n' > /original/vendor.conf && ssh -G -T snapshot.invalid | grep -qx 'port 2222' && ! ls -l /proc/self/fd | grep -q 'ttc-rail-config'`
			if denied {
				// Denied contents are never copied; the final file mask is
				// empty and owned by the launcher so SSH can still read it.
				spec.options.Policy.Denies = []string{"/etc/ssh/ssh_config.d/vendor.conf"}
				var err error
				selected, err = planSandbox(ctx, spec, "/")
				if err != nil {
					t.Fatal(err)
				}
				script = `test -L /etc/ssh/ssh_config.d/vendor.conf && ssh -G -T snapshot.invalid | grep -qx 'port 22'`
			}
			v, err := bubblewrapInvocation(*selected, sandboxProcess{command: []string{"/bin/sh", "-c", script}, environment: []string{"PATH=/usr/bin:/bin", "HOME=" + opts.Home}})
			if err != nil {
				t.Fatal(err)
			}
			cmd, closeFiles, err := v.prepareCommand(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer closeFiles()
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("snapshot namespace failed: %v: %s", err, out)
			}
		})
	}
}
