package rail

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// SandboxOptions describes host paths made available to a rail instance. All
// paths are absolute. DataDir, CacheDir and SocketDir must already exist;
// SocketDir is the private instance directory, not its registry parent.
type SandboxOptions struct {
	Workdir, Home, DataDir, CacheDir, ConfigHome, Executable, SocketDir, Hostname string
	Policy                                                                        Policy
}

// SandboxArgs returns Bubblewrap options, without a command or its separator.
// The sandbox shares host networking, but has a fresh filesystem, private
// process and hostname namespaces, and Bubblewrap's PID-1 reaper. It deliberately
// does not die with its caller: the persistent host supervisor owns its lifetime.
func SandboxArgs(opts SandboxOptions) ([]string, error) {
	return sandboxArgs(opts, "/", os.Getenv)
}

type sandboxMount struct {
	source, dest string
	writable     bool
	protect      bool // Configuration sources must not gain a writable alias.
}

type mountPlanner struct {
	root   string
	mounts []sandboxMount
	links  map[string]string
}

// hostRoot isolates filesystem fixtures; sandbox destinations remain absolute
// namespace paths. getenv is injected so tests never depend on the user's shell.
func sandboxArgs(opts SandboxOptions, hostRoot string, getenv func(string) string) ([]string, error) {
	p := mountPlanner{root: hostRoot, links: map[string]string{"/var/run": "/run"}}
	paths := []struct{ name, path string }{
		{"workdir", opts.Workdir}, {"home", opts.Home}, {"data directory", opts.DataDir},
		{"cache directory", opts.CacheDir}, {"config home", opts.ConfigHome},
		{"executable", opts.Executable}, {"socket directory", opts.SocketDir},
	}
	for _, item := range paths {
		if !filepath.IsAbs(item.path) || strings.ContainsRune(item.path, 0) {
			return nil, fmt.Errorf("rail %s must be an absolute path", item.name)
		}
	}
	opts.Workdir = filepath.Clean(opts.Workdir)
	opts.Home = filepath.Clean(opts.Home)
	if opts.Home == "/" || mountContains(opts.Workdir, opts.Home) {
		return nil, fmt.Errorf("rail workdir %q must not contain the private home %q", opts.Workdir, opts.Home)
	}
	for _, path := range []string{opts.Workdir, opts.Home, opts.DataDir, opts.CacheDir} {
		if err := checkMountDestination(path); err != nil {
			return nil, err
		}
	}
	if opts.Hostname == "" || strings.ContainsAny(opts.Hostname, "\x00\n\r") {
		return nil, fmt.Errorf("rail requires a nonempty host hostname")
	}

	defaults := map[string]sandboxMount{}
	add := func(source, dest string, writable, required, protect bool) error {
		source, info, err := p.source(source)
		if err != nil {
			if !required && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("rail mount %q: %w", dest, err)
		}
		if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("rail mount %q: source is not a directory, regular file or socket", dest)
		}
		defaults[filepath.Clean(dest)] = sandboxMount{source, filepath.Clean(dest), writable, protect}
		return nil
	}
	if err := add("/usr", "/usr", false, true, false); err != nil {
		return nil, err
	}
	for _, path := range []string{"/bin", "/sbin", "/lib", "/lib64"} {
		info, err := os.Lstat(p.host(path))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("rail inspect %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(p.host(path))
			if err != nil {
				return nil, err
			}
			resolved, err := p.resolveHost(path)
			if err != nil {
				return nil, err
			}
			if !mountContains("/usr", resolved) {
				return nil, fmt.Errorf("rail system link %s must resolve beneath /usr", path)
			}
			p.links[path] = target
		} else if err := add(path, path, false, false, false); err != nil {
			return nil, err
		}
	}
	if err := add("/etc", "/etc", false, false, false); err != nil {
		return nil, err
	}
	// systemd-managed resolv.conf often points into host /run, which is
	// otherwise private. Bind only the resolved resolver file, not host /run.
	if err := add("/etc/resolv.conf", "/etc/resolv.conf", false, false, false); err != nil {
		return nil, err
	}
	for _, path := range []string{opts.Workdir, opts.DataDir, opts.CacheDir} {
		_, info, err := p.source(path)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("rail writable directory %q must exist and be a directory", path)
		}
		if err := add(path, path, true, true, false); err != nil {
			return nil, err
		}
	}
	if err := add(opts.Executable, opts.Executable, false, true, true); err != nil {
		return nil, err
	}
	for _, name := range []string{".bashrc", ".bash_profile", ".bash_login", ".profile", ".zshenv", ".zprofile", ".zshrc", ".zlogin", ".zlogout", ".tmux.conf"} {
		path := filepath.Join(opts.Home, name)
		if err := add(path, path, false, false, true); err != nil {
			return nil, err
		}
	}
	configDirs := []string{filepath.Join(opts.Home, ".config", "zsh"), filepath.Join(opts.Home, ".config", "tmux"), filepath.Join(opts.ConfigHome, "zsh"), filepath.Join(opts.ConfigHome, "tmux"), filepath.Join(opts.ConfigHome, "ttc")}
	if zdot := getenv("ZDOTDIR"); zdot != "" {
		if !filepath.IsAbs(zdot) {
			return nil, fmt.Errorf("rail ZDOTDIR must be absolute")
		}
		zdot = filepath.Clean(zdot)
		// ZDOTDIR=$HOME needs only the dotfiles already selected above, not
		// the entire host home (including credentials and unrelated projects).
		if zdot != opts.Home {
			if mountContains(zdot, opts.Home) || mountContains(zdot, opts.Workdir) {
				return nil, fmt.Errorf("rail ZDOTDIR %q covers the private home or writable workdir", zdot)
			}
			configDirs = append(configDirs, zdot)
		}
	}
	for _, path := range configDirs {
		if err := add(path, path, false, false, true); err != nil {
			return nil, err
		}
	}
	// Individual entrypoints can be symlinks escaping a read-only config
	// directory. Resolve and protect their targets, not just the directory.
	for _, path := range []string{filepath.Join(opts.ConfigHome, "ttc", "web-search.json"), filepath.Join(opts.ConfigHome, "ttc", "rail.json"), filepath.Join(opts.Home, ".config", "tmux", "tmux.conf"), filepath.Join(opts.ConfigHome, "tmux", "tmux.conf")} {
		if err := add(path, path, false, false, true); err != nil {
			return nil, err
		}
	}
	// TTC discovers singular and plural skill directories, from root to cwd.
	for dir := filepath.Dir(opts.Workdir); ; dir = filepath.Dir(dir) {
		for _, relative := range []string{"AGENTS.md", ".agents/skill", ".agents/skills"} {
			path := filepath.Join(dir, relative)
			if err := add(path, path, false, false, false); err != nil {
				return nil, err
			}
		}
		if dir == "/" {
			break
		}
	}
	for _, name := range []string{"skill", "skills"} {
		path := filepath.Join(opts.Home, ".agents", name)
		if err := add(path, path, false, false, false); err != nil {
			return nil, err
		}
	}
	for _, mount := range opts.Policy.Mounts {
		if !filepath.IsAbs(mount.Source) {
			return nil, fmt.Errorf("rail mount source %q must be absolute", mount.Source)
		}
		if err := checkMountDestination(mount.Dest); err != nil {
			return nil, err
		}
		// Authorization gates access to the standard Docker endpoint, not just
		// the shorthand. Read-only socket binds still permit API connections.
		source, _, err := p.source(mount.Source)
		if err != nil {
			return nil, fmt.Errorf("rail mount %q: %w", mount.Dest, err)
		}
		dockerPath, err := p.resolveHost("/var/run/docker.sock")
		if err != nil {
			return nil, err
		}
		if !opts.Policy.Docker && mountContains(source, p.host(dockerPath)) {
			return nil, fmt.Errorf("rail mount %q exposes Docker; enable the globally authorized docker service first", mount.Source)
		}
		if err := add(mount.Source, mount.Dest, mount.Writable, true, false); err != nil {
			return nil, err
		}
	}
	if opts.Policy.Docker {
		if host := getenv("DOCKER_HOST"); host != "" && host != "unix:///var/run/docker.sock" {
			return nil, fmt.Errorf("rail Docker supports only unix:///var/run/docker.sock; DOCKER_HOST is %q", host)
		}
		_, info, err := p.source("/var/run/docker.sock")
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("rail Docker requires a socket at /var/run/docker.sock")
		}
		if err := add("/var/run/docker.sock", "/var/run/docker.sock", true, true, false); err != nil {
			return nil, err
		}
	}
	// SocketDir is mounted alone: its host parent contains supervisor metadata.
	_, socketInfo, err := p.source(opts.SocketDir)
	if err != nil || !socketInfo.IsDir() {
		return nil, fmt.Errorf("rail socket directory %q must exist and be a directory", opts.SocketDir)
	}
	if err := add(opts.SocketDir, "/run/ttc-rail", true, true, false); err != nil {
		return nil, err
	}

	args := []string{"--unshare-user", "--unshare-pid", "--unshare-uts", "--hostname", opts.Hostname + "-ttc", "--cap-drop", "ALL", "--new-session", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp"}
	// Workspaces can live under TTC scratch. Create its synthetic ancestors
	// with exact modes before Bubblewrap creates mountpoint parents (0755).
	args = append(args, "--perms", "1777", "--dir", "/tmp/ttc", "--perms", "0700", "--dir", filepath.Join("/tmp/ttc", strconv.Itoa(os.Geteuid())), "--tmpfs", "/run", "--tmpfs", opts.Home)
	linkPaths := make([]string, 0, len(p.links))
	for path := range p.links {
		linkPaths = append(linkPaths, path)
	}
	sort.Strings(linkPaths)
	for _, path := range linkPaths {
		args = append(args, "--symlink", p.links[path], path)
	}
	ordered := make([]sandboxMount, 0, len(defaults))
	for _, mount := range defaults {
		// Normalize the merged-/usr aliases before sorting: /bin/tool must
		// overlay /usr/bin/tool after /usr, not be hidden by the /usr bind.
		dest, err := p.resolveDest(mount.dest)
		if err != nil {
			return nil, err
		}
		mount.dest = dest
		ordered = append(ordered, mount)
	}
	sort.Slice(ordered, func(i, j int) bool { return mountLess(ordered[i].dest, ordered[j].dest) })
	if err := p.appendOrderedMounts(&args, ordered); err != nil {
		return nil, err
	}
	// Configuration files are mandatory read-only overlays, even when the user
	// explicitly allows their containing directory. Bind resolved sources and
	// protect aliases through writable host-directory mounts as well.
	var protected []sandboxMount
	for _, mount := range ordered {
		if mount.protect {
			protected = append(protected, mount)
		}
	}
	protectedFiles := append(append([]string(nil), opts.Policy.ConfigFiles...), opts.Executable)
	for _, path := range protectedFiles {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("rail config file %q must be absolute", path)
		}
		source, info, err := p.source(path)
		if err != nil {
			return nil, fmt.Errorf("rail config file %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("rail config file %q is not a regular file", path)
		}
		if err := checkMountDestination(path); err != nil {
			return nil, err
		}
		mount := sandboxMount{source: source, dest: path, protect: true}
		if err := p.appendMount(&args, mount); err != nil {
			return nil, err
		}
		protected = append(protected, mount)
	}
	aliases := append([]sandboxMount(nil), p.mounts...)
	for _, config := range protected {
		for _, alias := range aliases {
			if !alias.writable || !mountContains(alias.source, config.source) {
				continue
			}
			rel, _ := filepath.Rel(alias.source, config.source)
			mount := sandboxMount{source: config.source, dest: filepath.Join(alias.dest, rel)}
			if err := p.appendMount(&args, mount); err != nil {
				return nil, err
			}
		}
	}

	denies := make([]string, 0, len(opts.Policy.Denies))
	for _, path := range opts.Policy.Denies {
		if err := checkMountDestination(path); err != nil {
			return nil, fmt.Errorf("rail deny: %w", err)
		}
		path, err := p.resolveDest(path)
		if err != nil {
			return nil, err
		}
		if err := checkMountDestination(path); err != nil {
			return nil, fmt.Errorf("rail deny: %w", err)
		}
		if mountContains(path, opts.Workdir) {
			return nil, fmt.Errorf("rail deny %q covers writable workdir %q", path, opts.Workdir)
		}
		denies = append(denies, path)
	}
	sort.Slice(denies, func(i, j int) bool { return mountLess(denies[i], denies[j]) })
	var masked []string
	for _, path := range denies {
		covered := false
		for _, ancestor := range masked {
			if mountContains(ancestor, path) {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		info, err := p.visibleInfo(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("rail deny %q: %w", path, err)
		}
		if err == nil && !info.IsDir() {
			args = append(args, "--ro-bind", p.host("/dev/null"), path)
		} else {
			args = append(args, "--tmpfs", path, "--remount-ro", path)
		}
		masked = append(masked, path)
	}
	return append(args, "--chdir", opts.Workdir), nil
}

func checkMountDestination(path string) error {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return fmt.Errorf("rail mount destination %q must be absolute", path)
	}
	path = filepath.Clean(path)
	for _, reserved := range []string{"/proc", "/dev", "/run/ttc-rail"} {
		if mountContains(path, reserved) || mountContains(reserved, path) {
			return fmt.Errorf("rail path %q overlaps reserved path %q", path, reserved)
		}
	}
	return nil
}

func mountContains(parent, path string) bool {
	parent, path = filepath.Clean(parent), filepath.Clean(path)
	return parent == "/" || parent == path || strings.HasPrefix(path, parent+"/")
}

func mountLess(a, b string) bool {
	if mountContains(a, b) && a != b {
		return true
	}
	if mountContains(b, a) && a != b {
		return false
	}
	return a < b
}

func (p *mountPlanner) host(path string) string { return filepath.Join(p.root, path) }

func (p *mountPlanner) source(path string) (string, fs.FileInfo, error) {
	resolved, err := p.resolveHost(path)
	if err != nil {
		return "", nil, err
	}
	source := p.host(resolved)
	info, err := os.Stat(source)
	return source, info, err
}

func (p *mountPlanner) resolveHost(path string) (string, error) {
	return resolveMountLinks(path, func(path string) (string, error) {
		return readMountLink(p.host(path))
	})
}

func (p *mountPlanner) visibleSource(path string) string {
	for i := len(p.mounts) - 1; i >= 0; i-- {
		mount := p.mounts[i]
		if mountContains(mount.dest, path) {
			rel, _ := filepath.Rel(mount.dest, path)
			return filepath.Join(mount.source, rel)
		}
	}
	return ""
}

func (p *mountPlanner) visibleInfo(path string) (fs.FileInfo, error) {
	if source := p.visibleSource(path); source != "" {
		return os.Lstat(source)
	}
	return nil, fs.ErrNotExist
}

func (p *mountPlanner) resolveDest(path string) (string, error) {
	return resolveMountLinks(path, func(path string) (string, error) {
		if source := p.visibleSource(path); source != "" {
			return readMountLink(source)
		}
		return p.links[path], nil
	})
}

func (p *mountPlanner) appendOrderedMounts(args *[]string, ordered []sandboxMount) error {
	pending := append([]sandboxMount(nil), ordered...)
	for len(pending) > 0 {
		next := -1
		for i, mount := range pending {
			dest, err := p.resolveDest(mount.dest)
			if err != nil {
				return err
			}
			if mount.dest != "/run/ttc-rail" {
				if err := checkMountDestination(dest); err != nil {
					return err
				}
			}
			ready := true
			for j, ancestor := range pending {
				if i == j {
					continue
				}
				parent, err := p.resolveDest(ancestor.dest)
				if err != nil {
					return err
				}
				if parent == dest {
					return fmt.Errorf("rail mount destinations %q and %q resolve to the same path %q", mount.dest, ancestor.dest, dest)
				}
				if mountContains(parent, dest) || mountContains(ancestor.dest, mount.dest) {
					ready = false
					break
				}
			}
			if ready {
				next = i
				break
			}
		}
		if next < 0 {
			return fmt.Errorf("rail mount destinations have cyclic symlink dependencies")
		}
		if err := p.appendMount(args, pending[next]); err != nil {
			return err
		}
		pending = append(pending[:next], pending[next+1:]...)
	}
	return nil
}

func (p *mountPlanner) appendMount(args *[]string, mount sandboxMount) error {
	dest, err := p.resolveDest(mount.dest)
	if err != nil {
		return fmt.Errorf("rail mount destination %q: %w", mount.dest, err)
	}
	// A symlink in an allowed directory must not redirect an overlay into the
	// sandbox's proc, device or supervisor mounts.
	if mount.dest != "/run/ttc-rail" {
		if err := checkMountDestination(dest); err != nil {
			return err
		}
	}
	mount.dest = dest
	flag := "--ro-bind"
	if mount.writable {
		flag = "--bind"
	}
	*args = append(*args, flag, mount.source, mount.dest)
	p.mounts = append(p.mounts, mount)
	return nil
}

func readMountLink(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", nil
	}
	return os.Readlink(path)
}

func resolveMountLinks(path string, readlink func(string) (string, error)) (string, error) {
	path = filepath.Clean(path)
	for count := 0; count < 40; count++ {
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		prefix := "/"
		changed := false
		for i, part := range parts {
			prefix = filepath.Join(prefix, part)
			link, err := readlink(prefix)
			if err != nil {
				return "", err
			}
			if link == "" {
				continue
			}
			if !filepath.IsAbs(link) {
				link = filepath.Join(filepath.Dir(prefix), link)
			}
			path = filepath.Join(append([]string{link}, parts[i+1:]...)...)
			changed = true
			break
		}
		if !changed {
			return path, nil
		}
	}
	return "", fmt.Errorf("too many symlinks resolving %q", path)
}
