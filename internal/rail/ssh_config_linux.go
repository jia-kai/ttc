package rail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

const sshConfigDirectory = "/etc/ssh/ssh_config.d"

const (
	maxSSHConfigEntries    = 256
	maxSSHConfigBytes      = 1 << 20
	maxSSHConfigTotalBytes = 4 << 20
)

// hostDirectoryObservation records a bounded, sorted directory listing. Unlike
// ordinary mount inspection, snapshot selection depends on directory membership.
type hostDirectoryObservation struct {
	path, entries string // Entry names separated by NUL; names cannot contain NUL.
}

// snapshotSSHConfigs preserves the effective namespace's symlinks and replaces
// their regular-file targets with immutable, launcher-owned copies. It runs
// after primary/protective mounts, before the final deny masks.
func (p *mountPlanner) snapshotSSHConfigs(ctx context.Context, spec sandboxSpec) error {
	if !spec.sshDropins {
		return nil
	}
	p.sshConfigTargets = make(map[string]bool)
	var denies []string
	for _, path := range spec.options.Policy.Denies {
		resolved, err := p.resolveDest(ctx, path)
		if err != nil {
			return err
		}
		denies = append(denies, resolved)
	}
	denied := func(path string) bool {
		for _, deny := range denies {
			if mountContains(deny, path) {
				return true
			}
		}
		return false
	}
	directory, err := p.resolveDest(ctx, sshConfigDirectory)
	if err != nil {
		return fmt.Errorf("rail SSH drop-in directory: %w", err)
	}
	if denied(sshConfigDirectory) || denied(directory) {
		return nil
	}
	names := make(map[string]bool)
	if source := p.visibleSource(directory); source != "" {
		info, err := p.inspection.stat(ctx, source)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("rail SSH drop-in directory: %w", err)
		}
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("rail SSH drop-in directory %q is not a directory", directory)
			}
			observation, err := readSSHDirectory(ctx, source, info)
			if err != nil {
				return err
			}
			p.plan.sshDirectories = append(p.plan.sshDirectories, observation)
			for _, name := range strings.Split(observation.entries, "\x00") {
				if sshConfigName(name) {
					names[name] = true
				}
			}
			if _, err := p.inspection.stat(ctx, source); err != nil {
				return err
			}
		}
	}
	// File imports can add entries not present in the underlying directory.
	for _, mount := range p.mounts {
		if filepath.Dir(mount.dest) == directory && sshConfigName(filepath.Base(mount.dest)) {
			names[filepath.Base(mount.dest)] = true
		}
	}
	if len(names) > maxSSHConfigEntries {
		return fmt.Errorf("rail SSH drop-ins exceed %d entries", maxSSHConfigEntries)
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	seen := make(map[string]bool)
	total := 0
	for _, name := range ordered {
		path := filepath.Join(directory, name)
		dest, err := p.resolveDest(ctx, path)
		if err != nil {
			return fmt.Errorf("rail SSH drop-in %q: %w", path, err)
		}
		p.sshConfigTargets[dest] = true
		if denied(filepath.Join(sshConfigDirectory, name)) || denied(path) || denied(dest) || seen[dest] {
			continue
		}
		if err := checkMountDestination(dest); err != nil {
			return fmt.Errorf("rail SSH drop-in %q: %w", path, err)
		}
		source := p.visibleSource(dest)
		if source == "" {
			return fmt.Errorf("rail SSH drop-in %q has no visible file target", path)
		}
		info, err := p.inspection.stat(ctx, source)
		if err != nil {
			return fmt.Errorf("rail SSH drop-in %q: %w", path, err)
		}
		data, err := p.inspection.readSSHConfig(ctx, source, info, spec.environment.UID)
		if err != nil {
			return err
		}
		total += len(data)
		if total > maxSSHConfigTotalBytes {
			return fmt.Errorf("rail SSH drop-ins exceed %d total bytes", maxSSHConfigTotalBytes)
		}
		p.plan.filesystem = append(p.plan.filesystem, filesystemOperation{kind: filesystemFile, dest: dest, data: data})
		// The generated file has the same regular-file shape as its observed
		// source. Retain that source for namespace probes, never as its backing
		// storage: execution creates an anonymous copy with no staging alias.
		p.mounts = append(p.mounts, resolvedMount{source: source, dest: dest})
		seen[dest] = true
	}
	return ctx.Err()
}

func sshConfigName(name string) bool {
	return !strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".conf")
}

func (h *hostInspection) readSSHConfig(ctx context.Context, path string, info fs.FileInfo, uid int) (string, error) {
	fingerprint, err := fingerprintHostInfo(info)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || (fingerprint.uid != 0 && fingerprint.uid != uint32(uid)) || info.Mode().Perm()&0022 != 0 {
		return "", fmt.Errorf("rail SSH config %q must be a regular file owned by root or UID %d and not writable by group/others (owner %d, mode %04o)", path, uid, fingerprint.uid, info.Mode().Perm())
	}
	if info.Size() > maxSSHConfigBytes {
		return "", fmt.Errorf("rail SSH config %q exceeds %d bytes", path, maxSSHConfigBytes)
	}
	file, err := openObservedFile(ctx, path, info, unix.O_NONBLOCK)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxSSHConfigBytes+1))
	if err != nil {
		return "", fmt.Errorf("rail read SSH config %q: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(data) > maxSSHConfigBytes {
		return "", fmt.Errorf("rail SSH config %q exceeds %d bytes", path, maxSSHConfigBytes)
	}
	if err := checkObservedDescriptor(file, fingerprint); err != nil {
		return "", err
	}
	if _, err := h.stat(ctx, path); err != nil {
		return "", err
	}
	return string(data), nil
}

func readSSHDirectory(ctx context.Context, path string, info fs.FileInfo) (hostDirectoryObservation, error) {
	file, err := openObservedFile(ctx, path, info, unix.O_DIRECTORY|unix.O_NONBLOCK)
	if err != nil {
		return hostDirectoryObservation{}, err
	}
	defer file.Close()
	entries, err := file.ReadDir(maxSSHConfigEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return hostDirectoryObservation{}, fmt.Errorf("rail list SSH drop-ins %q: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return hostDirectoryObservation{}, err
	}
	if len(entries) > maxSSHConfigEntries {
		return hostDirectoryObservation{}, fmt.Errorf("rail SSH drop-in directory %q exceeds %d entries", path, maxSSHConfigEntries)
	}
	fingerprint, err := fingerprintHostInfo(info)
	if err != nil {
		return hostDirectoryObservation{}, err
	}
	if err := checkObservedDescriptor(file, fingerprint); err != nil {
		return hostDirectoryObservation{}, err
	}
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	sort.Strings(names)
	return hostDirectoryObservation{path: path, entries: strings.Join(names, "\x00")}, nil
}

// openObservedFile rejects a substituted final symlink and compares descriptor
// metadata with the planner's observation before reading any bytes or entries.
func openObservedFile(ctx context.Context, path string, info fs.FileInfo, flags int) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fingerprint, err := fingerprintHostInfo(info)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|flags, 0)
	if err != nil {
		return nil, fmt.Errorf("rail open observed SSH path %q: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if err := checkObservedDescriptor(file, fingerprint); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func checkObservedDescriptor(file *os.File, expected hostFingerprint) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	fingerprint, err := fingerprintHostInfo(info)
	if err != nil {
		return err
	}
	if fingerprint != expected {
		return hostChanged(hostObservationKey{path: file.Name(), follow: true})
	}
	return nil
}
