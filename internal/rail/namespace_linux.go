package rail

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Host inspection resolves links within the fixture root, which affects source
// paths only. Absolute link targets still name paths beneath that host root.
func (p *mountPlanner) host(path string) string { return filepath.Join(p.root, path) }

func (p *mountPlanner) source(ctx context.Context, path string) (string, fs.FileInfo, error) {
	resolved, err := p.resolveHost(ctx, path)
	if err != nil {
		return "", nil, err
	}
	source := p.host(resolved)
	info, err := p.inspection.stat(ctx, source)
	return source, info, err
}

func (p *mountPlanner) resolveHost(ctx context.Context, path string) (string, error) {
	return resolveMountLinks(ctx, path, func(path string) (string, error) {
		return p.readMountLink(ctx, p.host(path))
	})
}

// Namespace inspection follows the mounts already placed, newest first, and
// synthetic links where no bind is visible. It never falls back to host paths.
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

func (p *mountPlanner) visibleInfo(ctx context.Context, path string) (fs.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source := p.visibleSource(path); source != "" {
		return p.inspection.lstat(ctx, source)
	}
	return nil, fs.ErrNotExist
}

func (p *mountPlanner) resolveDest(ctx context.Context, path string) (string, error) {
	return resolveMountLinks(ctx, path, func(path string) (string, error) {
		if source := p.visibleSource(path); source != "" {
			return p.readMountLink(ctx, source)
		}
		return p.links[path], nil
	})
}

func (p *mountPlanner) readMountLink(ctx context.Context, path string) (string, error) {
	info, err := p.inspection.lstat(ctx, path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", nil
	}
	return p.inspection.readlink(ctx, path)
}

func resolveMountLinks(ctx context.Context, path string, readlink func(string) (string, error)) (string, error) {
	path = filepath.Clean(path)
	for count := 0; count < 40; count++ {
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		prefix := "/"
		changed := false
		for i, part := range parts {
			if err := ctx.Err(); err != nil {
				return "", err
			}
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
			if err := ctx.Err(); err != nil {
				return "", err
			}
			return path, nil
		}
	}
	return "", fmt.Errorf("too many symlinks resolving %q", path)
}
