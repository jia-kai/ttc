package rail

import (
	"context"
	"fmt"
	"path/filepath"
)

// defaultMounts is the central catalog of rail's default host imports and system
// aliases. It selects paths only, leaving filesystem inspection to the planner.
// Service sockets and explicit policy mounts are not defaults.
func defaultMounts(ctx context.Context, opts sandboxOptions, env sandboxEnvironment) ([]mountRequest, error) {
	mounts := []mountRequest{
		{Source: "/usr", Required: true},
		{Source: "/bin", Kind: mountSystemAlias},
		{Source: "/sbin", Kind: mountSystemAlias},
		{Source: "/lib", Kind: mountSystemAlias},
		{Source: "/lib64", Kind: mountSystemAlias},
		{Source: "/run", Dest: "/var/run", Kind: mountSymlink},
		{Source: "/etc"},
		// Import the resolved resolver file without exposing its host /run parent.
		{Source: "/etc/resolv.conf"},
		{Source: opts.Workdir, Writable: true, Required: true, Kind: mountWritableRoot},
		{Source: opts.DataDir, Writable: true, Required: true, Kind: mountWritableRoot},
		{Source: opts.CacheDir, Writable: true, Required: true, Kind: mountWritableRoot},
		{Source: opts.Executable, Required: true, Protect: true},
	}
	for _, name := range []string{".bashrc", ".bash_profile", ".bash_login", ".profile", ".zshenv", ".zprofile", ".zshrc", ".zlogin", ".zlogout", ".tmux.conf", ".gitconfig"} {
		mounts = append(mounts, mountRequest{Source: filepath.Join(opts.Home, name), Protect: true})
	}
	// Relative HISTFILE does not resolve against the launch cwd. Missing absolute
	// selections do not fall back and history is never created by importing it.
	history := env.HistoryFile
	if !filepath.IsAbs(history) {
		history = filepath.Join(opts.Home, ".zsh_history")
	}
	mounts = append(mounts, mountRequest{Source: history, Writable: true, Kind: mountHistory})
	for _, base := range []string{filepath.Join(opts.Home, ".config"), opts.ConfigHome} {
		for _, name := range []string{"zsh", "tmux", "nvim"} {
			mounts = append(mounts, mountRequest{Source: filepath.Join(base, name), Protect: true})
		}
	}
	mounts = append(mounts, mountRequest{Source: filepath.Join(opts.ConfigHome, "ttc"), Protect: true})
	if zdot := env.ZDotDir; zdot != "" {
		if err := absoluteRoot("rail ZDOTDIR", zdot); err != nil {
			return nil, err
		}
		zdot = filepath.Clean(zdot)
		// Home already has selected startup files; importing it wholesale would
		// expose credentials and unrelated workspaces.
		if zdot != opts.Home {
			if mountContains(zdot, opts.Home) || mountContains(zdot, opts.Workdir) {
				return nil, fmt.Errorf("rail ZDOTDIR %q covers the private home or writable workdir", zdot)
			}
			mounts = append(mounts, mountRequest{Source: zdot, Protect: true})
		}
	}
	// These entrypoints can escape imported directories through symlinks. The
	// planner protects their resolved targets as well as their named destinations.
	for _, path := range []string{
		filepath.Join(opts.ConfigHome, "ttc", "web-search.json"),
		filepath.Join(opts.ConfigHome, "ttc", "rail.json"),
		filepath.Join(opts.Home, ".config", "tmux", "tmux.conf"),
		filepath.Join(opts.ConfigHome, "tmux", "tmux.conf"),
	} {
		mounts = append(mounts, mountRequest{Source: path, Protect: true})
	}
	// TTC discovers both singular and plural skill directories from root to cwd.
	for dir := filepath.Dir(opts.Workdir); ; dir = filepath.Dir(dir) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, relative := range []string{"AGENTS.md", ".agents/skill", ".agents/skills"} {
			mounts = append(mounts, mountRequest{Source: filepath.Join(dir, relative)})
		}
		if dir == "/" {
			break
		}
	}
	for _, name := range []string{"skill", "skills"} {
		mounts = append(mounts, mountRequest{Source: filepath.Join(opts.Home, ".agents", name)})
	}
	// Import only the private control directory, never its supervisor parent.
	mounts = append(mounts, mountRequest{Source: opts.SocketDir, Dest: "/run/ttc-rail", Writable: true, Required: true, Kind: mountDirectory})
	for i := range mounts {
		if mounts[i].Dest == "" {
			mounts[i].Dest = mounts[i].Source
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return mounts, nil
}
