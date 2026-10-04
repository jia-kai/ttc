package rail

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const sshAgentDest = "/run/ssh-agent.sock"

type resolvedMount struct {
	source, dest string
	writable     bool
	protect      bool // Configuration sources must not gain a writable alias.
	origin       mountOrigin
}

type mountPlanner struct {
	root             string
	mounts           []resolvedMount
	links            map[string]string
	plan             sandboxPlan
	inspection       hostInspection
	sshConfigTargets map[string]bool // Selected canonical drop-ins, including denied files.
}

// serviceEndpoints retains canonical host paths and inode information for the
// common mount audit and SSH-agent deny propagation. agentPath is the original
// host-namespace path supplied by the launcher, not a fixture-rooted host path.
type serviceEndpoints struct {
	agentPath, agentSource, dockerSource string
	agentInfo, dockerInfo                fs.FileInfo
}

func (s serviceEndpoints) exposesAgent(source string, info fs.FileInfo) bool {
	return s.agentSource != "" && (mountContains(source, s.agentSource) ||
		(s.agentInfo != nil && info != nil && os.SameFile(info, s.agentInfo)))
}

// hostRoot isolates filesystem fixtures; sandbox destinations remain absolute
// namespace paths. Planning checks cancellation between filesystem operations;
// individual host syscalls cannot be interrupted by context cancellation.
func planSandbox(ctx context.Context, spec sandboxSpec, hostRoot string) (*sandboxPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := mountPlanner{root: hostRoot, links: make(map[string]string)}
	writableSources, err := p.inspectWritableRoots(ctx, spec.mounts)
	if err != nil {
		return nil, err
	}
	endpoints, err := p.inspectServices(ctx, spec.options.Policy, spec.environment)
	if err != nil {
		return nil, err
	}
	candidates, err := p.collectCandidates(ctx, spec, writableSources, endpoints)
	if err != nil {
		return nil, err
	}
	p.initializeNamespace(spec.options, spec.environment)
	ordered, err := p.orderCandidates(ctx, candidates)
	if err != nil {
		return nil, err
	}
	if err := p.placeOrderedMounts(ctx, ordered); err != nil {
		return nil, err
	}
	if err := p.protectMounts(ctx, spec, ordered); err != nil {
		return nil, err
	}
	if err := p.snapshotSSHConfigs(ctx, spec); err != nil {
		return nil, err
	}
	if err := p.applyDenies(ctx, spec.options, endpoints); err != nil {
		return nil, err
	}
	if err := p.selectTmuxConfig(ctx, spec.options); err != nil {
		return nil, err
	}
	p.plan.hostObservations = p.inspection.freeze()
	return &p.plan, nil
}

func (p *mountPlanner) inspectWritableRoots(ctx context.Context, requests []mountRequest) (map[string]bool, error) {
	// Optional directory imports that alias a required writable root retain the
	// root's access mode at both destinations. Explicit policy mounts and mandatory
	// config-file overlays retain their precedence.
	writableSources := make(map[string]bool)
	for _, mount := range requests {
		if mount.Kind != mountWritableRoot {
			continue
		}
		source, info, err := p.source(ctx, mount.Source)
		if err != nil {
			return nil, fmt.Errorf("rail writable directory %q: %w", mount.Source, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("rail writable directory %q must exist and be a directory", mount.Source)
		}
		writableSources[source] = true
	}
	return writableSources, nil
}

func (p *mountPlanner) inspectServices(ctx context.Context, policy Policy, env sandboxEnvironment) (serviceEndpoints, error) {
	s := serviceEndpoints{agentPath: env.SSHAuthSock}
	if s.agentPath != "" {
		if err := absoluteRoot("SSH_AUTH_SOCK", s.agentPath); err != nil {
			return s, fmt.Errorf("rail %w", err)
		}
		var err error
		s.agentSource, s.agentInfo, err = p.source(ctx, s.agentPath)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return s, fmt.Errorf("rail SSH_AUTH_SOCK: %w", err)
		}
	}
	if policy.SSHAgent && (s.agentInfo == nil || s.agentInfo.Mode()&os.ModeSocket == 0) {
		return s, fmt.Errorf("rail ssh-agent requires SSH_AUTH_SOCK to name an existing absolute socket")
	}
	if s.agentInfo != nil && s.agentInfo.Mode()&os.ModeSocket != 0 {
		if err := checkSingleLink(s.agentInfo); err != nil {
			return s, fmt.Errorf("rail SSH_AUTH_SOCK: %w", err)
		}
	}
	var err error
	s.dockerSource, s.dockerInfo, err = p.source(ctx, "/var/run/docker.sock")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return s, fmt.Errorf("rail Docker endpoint: %w", err)
	}
	if s.dockerInfo != nil && s.dockerInfo.Mode()&os.ModeSocket != 0 {
		if err := checkSingleLink(s.dockerInfo); err != nil {
			return s, fmt.Errorf("rail Docker endpoint: %w", err)
		}
	}
	if policy.Docker {
		if host := env.DockerHost; host != "" && host != "unix:///var/run/docker.sock" {
			return s, fmt.Errorf("rail Docker supports only unix:///var/run/docker.sock; DOCKER_HOST is %q", host)
		}
		if s.dockerInfo == nil || s.dockerInfo.Mode()&os.ModeSocket == 0 {
			return s, fmt.Errorf("rail Docker requires a socket at /var/run/docker.sock")
		}
	}
	return s, nil
}

func (p *mountPlanner) collectCandidates(ctx context.Context, spec sandboxSpec, writableSources map[string]bool, endpoints serviceEndpoints) (map[string]resolvedMount, error) {
	candidates := make(map[string]resolvedMount)
	for _, mount := range spec.mounts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch mount.Kind {
		case mountSymlink:
			p.links[mount.Dest] = mount.Source
			continue
		case mountSystemAlias:
			handled, err := p.inspectSystemAlias(ctx, mount)
			if err != nil {
				return nil, err
			}
			if handled {
				continue
			}
		}
		if err := p.addCandidate(ctx, mount, spec.options.Policy, writableSources, endpoints, candidates); err != nil {
			return nil, err
		}
	}
	return candidates, nil
}

// inspectSystemAlias handles absent imports and merged-/usr symlinks. Other
// sources continue through the common mount validation and service audit.
func (p *mountPlanner) inspectSystemAlias(ctx context.Context, mount mountRequest) (bool, error) {
	info, err := p.inspection.lstat(ctx, p.host(mount.Source))
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("rail inspect %s: %w", mount.Source, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return false, nil
	}
	target, err := p.inspection.readlink(ctx, p.host(mount.Source))
	if err != nil {
		return false, err
	}
	resolved, err := p.resolveHost(ctx, mount.Source)
	if err != nil {
		return false, err
	}
	if !mountContains("/usr", resolved) {
		return false, fmt.Errorf("rail system link %s must resolve beneath /usr", mount.Source)
	}
	p.links[mount.Dest] = target
	return true, nil
}

func (p *mountPlanner) addCandidate(ctx context.Context, request mountRequest, policy Policy, writableSources map[string]bool, endpoints serviceEndpoints, candidates map[string]resolvedMount) error {
	dest, writable, protect := request.Dest, request.Writable, request.Protect
	source, info, err := p.source(ctx, request.Source)
	if err != nil {
		if !request.Required && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if request.Kind == mountHistory {
			return fmt.Errorf("rail zsh history %q: %w", request.Source, err)
		}
		return fmt.Errorf("rail mount %q: %w", dest, err)
	}
	if request.Kind == mountDirectory && !info.IsDir() {
		return fmt.Errorf("rail socket directory %q must exist and be a directory", request.Source)
	}
	if request.Kind == mountHistory {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("rail zsh history %q must be a regular file", request.Source)
		}
		if err := checkMountDestination(dest); err != nil {
			return err
		}
	}
	if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("rail mount %q: source is not a directory, regular file or socket", dest)
	}
	if request.Origin == mountDefault && !request.Required && !writable && info.IsDir() && writableSources[source] {
		writable, protect = true, false
	}
	if protect && info.Mode().IsRegular() {
		if err := checkSingleLink(info); err != nil {
			return fmt.Errorf("rail protected file %q: %w", dest, err)
		}
	}
	// Audit every mount, including defaults: an agent can live in the workdir,
	// cache, shell configuration or supervisor directory rather than /tmp.
	if !policy.SSHAgent && endpoints.exposesAgent(source, info) {
		return fmt.Errorf("rail mount %q exposes SSH_AUTH_SOCK; enable the globally authorized ssh-agent service first", dest)
	}
	// Read-only socket binds still permit API connections; canonical containment
	// and inode identity cover parent and direct-source aliases without walks.
	if !policy.Docker && (mountContains(source, endpoints.dockerSource) ||
		(endpoints.dockerInfo != nil && os.SameFile(info, endpoints.dockerInfo))) {
		return fmt.Errorf("rail mount %q exposes Docker; enable the globally authorized docker service first", dest)
	}
	dest = filepath.Clean(dest)
	candidates[dest] = resolvedMount{source: source, dest: dest, writable: writable, protect: protect, origin: request.Origin}
	return nil
}

func (p *mountPlanner) initializeNamespace(opts sandboxOptions, env sandboxEnvironment) {
	// Workspaces can live under TTC scratch. Create its synthetic ancestors
	// with exact modes before Bubblewrap creates mountpoint parents (0755).
	p.plan = sandboxPlan{
		hostname: opts.Hostname + "-ttc", workdir: opts.Workdir,
		namespaces: []namespaceKind{namespaceUser, namespaceProcess, namespaceHostname},
		newSession: true, dropCapabilities: true,
		environment: []environmentChange{
			{name: "PATH", value: filepath.Dir(opts.Executable) + ":" + env.Path},
			{name: "TTC_DATA_DIR", value: opts.DataDir},
			{name: "XDG_RUNTIME_DIR", value: "/run/ttc-rail"},
		},
		filesystem: []filesystemOperation{
			{kind: filesystemProc, dest: "/proc"},
			{kind: filesystemDevices, dest: "/dev"},
			{kind: filesystemTmpfs, dest: "/tmp", writable: true},
			{kind: filesystemDirectory, dest: "/tmp/ttc", mode: fs.ModeSticky | 0777},
			{kind: filesystemDirectory, dest: filepath.Join("/tmp/ttc", strconv.Itoa(env.UID)), mode: 0700},
			{kind: filesystemTmpfs, dest: "/run", writable: true},
			{kind: filesystemTmpfs, dest: opts.Home, writable: true},
		},
	}
	linkPaths := make([]string, 0, len(p.links))
	for path := range p.links {
		linkPaths = append(linkPaths, path)
	}
	sort.Strings(linkPaths)
	for _, path := range linkPaths {
		p.plan.filesystem = append(p.plan.filesystem, filesystemOperation{kind: filesystemSymlink, source: p.links[path], dest: path})
	}
}

func (p *mountPlanner) orderCandidates(ctx context.Context, candidates map[string]resolvedMount) ([]resolvedMount, error) {
	ordered := make([]resolvedMount, 0, len(candidates))
	for _, mount := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Keep the requested path: an ancestor bind can replace a synthetic
		// alias before this mount's destination is resolved for placement.
		ordered = append(ordered, mount)
	}
	sort.Slice(ordered, func(i, j int) bool { return mountLess(ordered[i].dest, ordered[j].dest) })
	return ordered, nil
}

func (p *mountPlanner) protectMounts(ctx context.Context, spec sandboxSpec, ordered []resolvedMount) error {
	// Configuration files are mandatory read-only overlays, even when the user
	// explicitly allows their containing directory. Bind resolved sources and
	// protect aliases through writable host-directory mounts as well.
	var protected []resolvedMount
	for _, mount := range ordered {
		if mount.protect {
			protected = append(protected, mount)
		}
	}
	protectedFiles := append([]string(nil), spec.options.Policy.ConfigFiles...)
	for _, mount := range spec.mounts {
		if mount.Required && mount.Protect {
			protectedFiles = appendUnique(protectedFiles, mount.Source)
		}
	}
	for _, path := range protectedFiles {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("rail config file %q must be absolute", path)
		}
		source, info, err := p.source(ctx, path)
		if err != nil {
			return fmt.Errorf("rail config file %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("rail config file %q is not a regular file", path)
		}
		if err := checkSingleLink(info); err != nil {
			return fmt.Errorf("rail protected file %q: %w", path, err)
		}
		if err := checkMountDestination(path); err != nil {
			return err
		}
		mount := resolvedMount{source: source, dest: path, protect: true}
		if err := p.placeMount(ctx, mount); err != nil {
			return err
		}
		protected = append(protected, mount)
	}
	aliases := append([]resolvedMount(nil), p.mounts...)
	for _, config := range protected {
		for _, alias := range aliases {
			if !alias.writable || !mountContains(alias.source, config.source) {
				continue
			}
			rel, _ := filepath.Rel(alias.source, config.source)
			mount := resolvedMount{source: config.source, dest: filepath.Join(alias.dest, rel)}
			if err := p.placeMount(ctx, mount); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *mountPlanner) applyDenies(ctx context.Context, opts sandboxOptions, endpoints serviceEndpoints) error {
	denies := append([]string(nil), opts.Policy.Denies...)
	agentDenied := false
	if opts.Policy.SSHAgent {
		for _, path := range opts.Policy.Denies {
			dest, err := p.resolveDest(ctx, path)
			if err != nil {
				return err
			}
			hostSource, info, err := p.source(ctx, path)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("rail deny %q: %w", path, err)
			}
			// A deny of the host endpoint must also cover the service's stable
			// destination and every directory/direct-source alias of that socket.
			if mountContains(dest, sshAgentDest) || mountContains(path, endpoints.agentPath) || endpoints.exposesAgent(hostSource, info) {
				agentDenied = true
			}
		}
		if agentDenied {
			for _, mount := range p.mounts {
				if err := ctx.Err(); err != nil {
					return err
				}
				info, err := p.inspection.stat(ctx, mount.source)
				if err != nil {
					return err
				}
				if endpoints.exposesAgent(mount.source, info) {
					alias := mount.source
					if info.IsDir() {
						alias = endpoints.agentSource
					}
					rel, _ := filepath.Rel(mount.source, alias)
					denies = appendUnique(denies, filepath.Join(mount.dest, rel))
				}
			}
		}
	}
	if opts.Policy.SSHAgent && !agentDenied {
		p.plan.environment = append(p.plan.environment, environmentChange{name: "SSH_AUTH_SOCK", value: sshAgentDest})
	} else {
		p.plan.environment = append(p.plan.environment, environmentChange{name: "SSH_AUTH_SOCK", unset: true})
	}
	// Compare deny masks and the workdir in the same final namespace. An
	// imported ancestor can redirect the originally requested workdir path.
	workdir, err := p.resolveDest(ctx, opts.Workdir)
	if err != nil {
		return fmt.Errorf("rail workdir %q: %w", opts.Workdir, err)
	}
	resolved, err := p.resolveDenies(ctx, denies, workdir)
	if err != nil {
		return err
	}
	p.plan.workdir = workdir
	return p.maskDenies(ctx, resolved)
}

func (p *mountPlanner) resolveDenies(ctx context.Context, denies []string, workdir string) ([]string, error) {
	resolved := make([]string, 0, len(denies))
	for _, path := range denies {
		if err := checkMountDestination(path); err != nil {
			return nil, fmt.Errorf("rail deny: %w", err)
		}
		path, err := p.resolveDest(ctx, path)
		if err != nil {
			return nil, err
		}
		if err := checkMountDestination(path); err != nil {
			return nil, fmt.Errorf("rail deny: %w", err)
		}
		if mountContains(path, workdir) {
			return nil, fmt.Errorf("rail deny %q covers writable workdir %q", path, workdir)
		}
		resolved = append(resolved, path)
	}
	sort.Slice(resolved, func(i, j int) bool { return mountLess(resolved[i], resolved[j]) })
	return resolved, nil
}

// Masks are the final filesystem mutations, so neither writable imports nor
// protective overlays can expose a denied path again.
func (p *mountPlanner) maskDenies(ctx context.Context, resolved []string) error {
	var masked []string
	for _, path := range resolved {
		if err := ctx.Err(); err != nil {
			return err
		}
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
		info, err := p.visibleInfo(ctx, path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("rail deny %q: %w", path, err)
		}
		if err == nil && !info.IsDir() {
			if p.sshConfigTargets[path] {
				// OpenSSH checks included files' owners even when they are
				// empty. A host /dev/null bind can have an unmapped owner.
				p.plan.filesystem = append(p.plan.filesystem, filesystemOperation{kind: filesystemFile, dest: path})
				masked = append(masked, path)
				continue
			}
			source, _, err := p.source(ctx, "/dev/null")
			if err != nil {
				return fmt.Errorf("rail deny mask source: %w", err)
			}
			p.plan.filesystem = append(p.plan.filesystem, filesystemOperation{kind: filesystemBind, source: source, dest: path})
		} else {
			p.plan.filesystem = append(p.plan.filesystem, filesystemOperation{kind: filesystemTmpfs, dest: path})
		}
		masked = append(masked, path)
	}
	return nil
}

func (p *mountPlanner) selectTmuxConfig(ctx context.Context, opts sandboxOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, config := range []string{filepath.Join(opts.Home, ".tmux.conf"), filepath.Join(opts.ConfigHome, "tmux", "tmux.conf")} {
		_, _, err := p.source(ctx, config)
		if err == nil {
			p.plan.tmuxConfig = config
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("rail tmux configuration %q: %w", config, err)
		}
	}
	return nil
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

// mountDependencies records prerequisite indices in the unchanged logical
// candidate list. Edges only accumulate, even when placement is replayed.
type mountDependencies []map[int]struct{}

func (d mountDependencies) add(parent, child int) bool {
	if d[child] == nil {
		d[child] = make(map[int]struct{})
	}
	if _, exists := d[child][parent]; exists {
		return false
	}
	d[child][parent] = struct{}{}
	return true
}

func (d mountDependencies) ready(index int, placed []bool) bool {
	for parent := range d[index] {
		if !placed[parent] {
			return false
		}
	}
	return true
}

func (p *mountPlanner) placeOrderedMounts(ctx context.Context, ordered []resolvedMount) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dependencies := make(mountDependencies, len(ordered))
	for child, mount := range ordered {
		for parent, ancestor := range ordered {
			if ancestor.dest != mount.dest && mountContains(ancestor.dest, mount.dest) {
				dependencies.add(parent, child)
			}
		}
	}
	baseMounts, baseOperations := len(p.mounts), len(p.plan.filesystem)
	for {
		// A newly discovered canonical ancestor invalidates tentative child
		// binds. Replay from the synthetic namespace, retaining logical paths
		// and dependencies rather than resolving already-normalized paths again.
		p.mounts = p.mounts[:baseMounts]
		p.plan.filesystem = p.plan.filesystem[:baseOperations]
		placed := make([]bool, len(ordered))
		// Only actual primary mutations claim destinations. Protection overlays
		// remain a separate phase and intentionally may replace primary binds.
		claimed := make(map[string]int)
		replay := false
		for remaining := len(ordered); remaining > 0; remaining-- {
			if err := ctx.Err(); err != nil {
				return err
			}
			next := -1
			var resolved resolvedMount
			var firstErr error
			coalesced := false
			for i, mount := range ordered {
				// In particular, never inspect a descendant of a pending logical
				// ancestor: that ancestor may hide a loop or replace an alias.
				if placed[i] || !dependencies.ready(i, placed) {
					continue
				}
				candidate, err := p.resolveMount(ctx, mount)
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				if previous, exists := claimed[candidate.dest]; exists {
					normalizedPrevious := ordered[previous]
					normalizedPrevious.dest = candidate.dest
					// Identical default aliases do not mutate the namespace and
					// must not introduce ancestor edges or cause a replay.
					if candidate.origin != mountDefault || candidate != normalizedPrevious {
						if firstErr == nil {
							firstErr = fmt.Errorf("rail mount destinations %q and %q resolve to the same path %q", ordered[previous].dest, mount.dest, candidate.dest)
						}
						continue
					}
					coalesced = true
				} else {
					for dest, previous := range claimed {
						if mountContains(candidate.dest, dest) {
							replay = dependencies.add(i, previous) || replay
						} else if mountContains(dest, candidate.dest) {
							dependencies.add(previous, i)
						}
					}
				}
				if replay {
					break
				}
				next, resolved = i, candidate
				break
			}
			if replay {
				break
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if next < 0 {
				// Ready candidates can be temporarily unresolvable (or collide)
				// until another bind changes the namespace. Only fail once no
				// successful placement is possible; dependency cycles fail loud.
				if firstErr != nil {
					return firstErr
				}
				return fmt.Errorf("rail mount destinations have cyclic symlink dependencies")
			}
			if !coalesced {
				p.appendMount(resolved)
				claimed[resolved.dest] = next
			}
			placed[next] = true
		}
		if !replay {
			return nil
		}
		// Every replay adds a previously unknown edge among a finite number
		// of candidates. A cycle stops placement instead of retrying forever.
	}
}

func (p *mountPlanner) resolveMount(ctx context.Context, mount resolvedMount) (resolvedMount, error) {
	dest, err := p.resolveDest(ctx, mount.dest)
	if err != nil {
		return resolvedMount{}, fmt.Errorf("rail mount destination %q: %w", mount.dest, err)
	}
	// A symlink in an allowed directory must not redirect an overlay into the
	// sandbox's proc, device or supervisor mounts.
	if mount.dest != "/run/ttc-rail" {
		if err := checkMountDestination(dest); err != nil {
			return resolvedMount{}, err
		}
	}
	mount.dest = dest
	return mount, nil
}

func (p *mountPlanner) placeMount(ctx context.Context, mount resolvedMount) error {
	mount, err := p.resolveMount(ctx, mount)
	if err != nil {
		return err
	}
	p.appendMount(mount)
	return nil
}

func (p *mountPlanner) appendMount(mount resolvedMount) {
	p.plan.filesystem = append(p.plan.filesystem, filesystemOperation{kind: filesystemBind, source: mount.source, dest: mount.dest, writable: mount.writable})
	p.mounts = append(p.mounts, mount)
}
