package rail

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// sandboxOptions are host locations selected by the caller. Data/cache/control
// directories must exist before planning; the control directory is private to
// this instance, not its registry parent.
type sandboxOptions struct {
	Workdir, Home, DataDir, CacheDir, ConfigHome, Executable, SocketDir, Hostname string
	Policy                                                                        Policy
}

// sandboxEnvironment captures only the host values affecting sandbox planning.
// Arbitrary inherited variables belong to sandboxProcess.environment, not here.
type sandboxEnvironment struct {
	SSHAuthSock, DockerHost, HistoryFile, ZDotDir, Path string
	UID                                                 int
}

// planningEnvironment projects an already captured process environment. It does
// not read live process state or replace the arbitrary variables inherited by
// the sandbox process.
func planningEnvironment(environment []string, uid int) sandboxEnvironment {
	env := sandboxEnvironment{UID: uid}
	for _, entry := range environment {
		name, value, _ := strings.Cut(entry, "=")
		switch name {
		case "SSH_AUTH_SOCK":
			env.SSHAuthSock = value
		case "DOCKER_HOST":
			env.DockerHost = value
		case "HISTFILE":
			env.HistoryFile = value
		case "ZDOTDIR":
			env.ZDotDir = value
		case "PATH":
			env.Path = value
		}
	}
	return env
}

type mountOrigin uint8

const (
	mountDefault mountOrigin = iota
	mountExplicit
	mountService
)

type mountRequestKind uint8

const (
	mountBind         mountRequestKind = iota
	mountWritableRoot                  // Required directory whose default aliases stay writable.
	mountDirectory                     // Required directory without writable-alias precedence.
	mountHistory                       // Optional existing regular Zsh history file.
	mountSystemAlias                   // Directory or merged-/usr symlink, inspected by the planner.
	mountSymlink                       // Synthetic alias; Source is its literal target.
)

// mountRequest is an unresolved import intent. Origin determines replacement and
// coalescing rules; Kind determines required source checks. Protect prevents
// writable source aliases after primary mounts have been placed.
type mountRequest struct {
	Source, Dest                string
	Writable, Required, Protect bool
	Kind                        mountRequestKind
	Origin                      mountOrigin
}

// sandboxSpec owns unresolved intents and captured inputs. It contains no host
// file information, canonical sources, execution order, or backend arguments.
type sandboxSpec struct {
	options     sandboxOptions
	environment sandboxEnvironment
	mounts      []mountRequest
	sshDropins  bool // Snapshot effective system client drop-ins; no source discovery here.
}

// resolveSandboxSpec expands the central defaults and config policy into one
// request stream. It performs syntax checks only; planSandbox owns filesystem
// validation and authoritative namespace-aware deny enforcement.
func resolveSandboxSpec(ctx context.Context, opts sandboxOptions, env sandboxEnvironment) (sandboxSpec, error) {
	if err := ctx.Err(); err != nil {
		return sandboxSpec{}, err
	}
	if err := checkPolicySize(opts.Policy); err != nil {
		return sandboxSpec{}, err
	}
	paths := []struct{ name, path string }{
		{"workdir", opts.Workdir}, {"home", opts.Home}, {"data directory", opts.DataDir},
		{"cache directory", opts.CacheDir}, {"config home", opts.ConfigHome},
		{"executable", opts.Executable}, {"socket directory", opts.SocketDir},
	}
	for _, item := range paths {
		if !filepath.IsAbs(item.path) || strings.ContainsRune(item.path, 0) {
			return sandboxSpec{}, fmt.Errorf("rail %s must be an absolute path", item.name)
		}
	}
	opts.Workdir = filepath.Clean(opts.Workdir)
	opts.Home = filepath.Clean(opts.Home)
	if opts.Home == "/" || mountContains(opts.Workdir, opts.Home) {
		return sandboxSpec{}, fmt.Errorf("rail workdir %q must not contain the private home %q", opts.Workdir, opts.Home)
	}
	for _, path := range []string{opts.Workdir, opts.Home, opts.DataDir, opts.CacheDir} {
		if err := checkMountDestination(path); err != nil {
			return sandboxSpec{}, err
		}
	}
	if opts.Hostname == "" || strings.ContainsAny(opts.Hostname, "\x00\n\r") {
		return sandboxSpec{}, fmt.Errorf("rail requires a nonempty host hostname")
	}
	if env.UID < 0 {
		return sandboxSpec{}, fmt.Errorf("rail requires a nonnegative host UID")
	}
	mounts, err := defaultMounts(ctx, opts, env)
	if err != nil {
		return sandboxSpec{}, err
	}
	for _, mount := range opts.Policy.Mounts {
		if err := absoluteRoot("rail mount source", mount.Source); err != nil {
			return sandboxSpec{}, err
		}
		if err := checkMountDestination(mount.Dest); err != nil {
			return sandboxSpec{}, err
		}
		mounts = append(mounts, mountRequest{Source: mount.Source, Dest: mount.Dest, Writable: mount.Writable, Required: true, Origin: mountExplicit})
	}
	if opts.Policy.Docker {
		mounts = append(mounts, mountRequest{Source: "/var/run/docker.sock", Dest: "/var/run/docker.sock", Writable: true, Required: true, Origin: mountService})
	}
	if opts.Policy.SSHAgent {
		mounts = append(mounts, mountRequest{Source: env.SSHAuthSock, Dest: sshAgentDest, Writable: true, Required: true, Origin: mountService})
	}
	// The specification owns all policy slices; later caller mutation cannot
	// alter a request or its deny/protection decisions.
	opts.Policy.Mounts = nil // Consumed into the owned request stream above.
	opts.Policy.Denies = append([]string(nil), opts.Policy.Denies...)
	opts.Policy.ConfigFiles = append([]string(nil), opts.Policy.ConfigFiles...)
	return sandboxSpec{options: opts, environment: env, mounts: mounts, sshDropins: true}, nil
}
