package rail

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"syscall"
)

// hostInspection records only paths inspected by planning, not directory trees.
// Its zero value is usable. Planning owns it; it is not safe for concurrent use.
type hostInspection struct {
	observations map[hostObservationKey]hostFingerprint
}

// Following and non-following probes have different meanings for symlinks.
type hostObservationKey struct {
	path   string
	follow bool
}

type hostObservation struct {
	key         hostObservationKey
	fingerprint hostFingerprint
}

// Keep scalar metadata rather than retaining caller-visible FileInfo values.
// Directory size, timestamps and link count describe child activity, not mount identity.
// Regular-file timestamps detect bounded content changes without reading content.
type hostFingerprint struct {
	exists              bool
	dev, ino, nlink     uint64
	rdev                uint64
	mode                fs.FileMode
	uid, gid            uint32
	size                int64
	mtimeSec, mtimeNsec int64
	ctimeSec, ctimeNsec int64
	link                string
}

func (h *hostInspection) lstat(ctx context.Context, path string) (fs.FileInfo, error) {
	return h.inspect(ctx, hostObservationKey{path: path})
}

func (h *hostInspection) stat(ctx context.Context, path string) (fs.FileInfo, error) {
	return h.inspect(ctx, hostObservationKey{path: path, follow: true})
}

// readlink observes both the link's identity and its literal target. lstat also
// captures the target when it encounters a link, even if the caller only uses mode.
func (h *hostInspection) readlink(ctx context.Context, path string) (string, error) {
	info, err := h.lstat(ctx, path)
	if err != nil {
		return "", err
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		return "", &os.PathError{Op: "readlink", Path: path, Err: syscall.EINVAL}
	}
	return h.observations[hostObservationKey{path: path}].link, nil
}

func (h *hostInspection) inspect(ctx context.Context, key hostObservationKey) (fs.FileInfo, error) {
	fingerprint, info, err := inspectHostPath(ctx, key)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if old, ok := h.observations[key]; ok && old != fingerprint {
		return nil, hostChanged(key)
	}
	// Stat and Lstat must agree on a non-symlink. Cross-check the two forms
	// rather than allowing an intervening replacement to become separate facts.
	otherKey := hostObservationKey{path: key.path, follow: !key.follow}
	if other, ok := h.observations[otherKey]; ok {
		nofollow := fingerprint
		if key.follow {
			nofollow = other
		}
		if nofollow.mode&fs.ModeSymlink == 0 && other != fingerprint {
			return nil, hostChanged(key)
		}
	}
	if h.observations == nil {
		h.observations = make(map[hostObservationKey]hostFingerprint)
	}
	h.observations[key] = fingerprint
	return info, err
}

// freeze returns a sorted, owned copy; subsequent observations cannot change it.
func (h *hostInspection) freeze() []hostObservation {
	observations := make([]hostObservation, 0, len(h.observations))
	for key, fingerprint := range h.observations {
		observations = append(observations, hostObservation{key: key, fingerprint: fingerprint})
	}
	sort.Slice(observations, func(i, j int) bool {
		a, b := observations[i].key, observations[j].key
		if a.path != b.path {
			return a.path < b.path
		}
		return !a.follow && b.follow
	})
	return observations
}

// validateHost checks the bounded facts captured by planning against the host.
// It does not read process environment, discover new paths, pin file descriptors,
// or prevent changes after validation and before mounts are executed.
func (p *sandboxPlan) validateHost(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, observation := range p.hostObservations {
		fingerprint, _, err := inspectHostPath(ctx, observation.key)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("rail revalidate host %q: %w", observation.key.path, err)
		}
		if fingerprint != observation.fingerprint {
			return hostChanged(observation.key)
		}
	}
	return ctx.Err()
}

func hostChanged(key hostObservationKey) error {
	return fmt.Errorf("rail host path %q changed during planning or before launch (follow symlinks: %t)", key.path, key.follow)
}

// Individual syscalls cannot be interrupted; check cancellation between them.
// A second Lstat bounds the metadata/readlink race for symlink observations.
func inspectHostPath(ctx context.Context, key hostObservationKey) (hostFingerprint, fs.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return hostFingerprint{}, nil, err
	}
	var info fs.FileInfo
	var err error
	if key.follow {
		info, err = os.Stat(key.path)
	} else {
		info, err = os.Lstat(key.path)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return hostFingerprint{}, nil, ctxErr
	}
	if err != nil {
		return hostFingerprint{}, nil, err
	}
	fingerprint, err := fingerprintHostInfo(info)
	if err != nil {
		return hostFingerprint{}, nil, fmt.Errorf("rail inspect host %q: %w", key.path, err)
	}
	if !key.follow && info.Mode()&fs.ModeSymlink != 0 {
		link, err := os.Readlink(key.path)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return hostFingerprint{}, nil, ctxErr
		}
		if err != nil {
			// A disappearing link is a changed observation, not an optional absence.
			if errors.Is(err, fs.ErrNotExist) {
				return hostFingerprint{}, nil, hostChanged(key)
			}
			return hostFingerprint{}, nil, fmt.Errorf("rail read observed symlink %q: %w", key.path, err)
		}
		again, err := os.Lstat(key.path)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return hostFingerprint{}, nil, ctxErr
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return hostFingerprint{}, nil, hostChanged(key)
			}
			return hostFingerprint{}, nil, fmt.Errorf("rail recheck observed symlink %q: %w", key.path, err)
		}
		second, err := fingerprintHostInfo(again)
		if err != nil {
			return hostFingerprint{}, nil, err
		}
		if second != fingerprint {
			return hostFingerprint{}, nil, hostChanged(key)
		}
		fingerprint.link = link
	}
	return fingerprint, info, nil
}

func fingerprintHostInfo(info fs.FileInfo) (hostFingerprint, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return hostFingerprint{}, fmt.Errorf("host metadata lacks Linux stat information")
	}
	fingerprint := hostFingerprint{
		exists: true,
		dev:    uint64(stat.Dev), ino: stat.Ino, rdev: uint64(stat.Rdev),
		mode: info.Mode(), uid: stat.Uid, gid: stat.Gid,
	}
	if !info.IsDir() {
		// Non-directory link counts are relevant to hardlink audits. Directory
		// counts can change when unrelated child directories are created or removed.
		fingerprint.nlink = uint64(stat.Nlink)
	}
	if info.Mode().IsRegular() {
		fingerprint.size = stat.Size
		fingerprint.mtimeSec, fingerprint.mtimeNsec = int64(stat.Mtim.Sec), int64(stat.Mtim.Nsec)
		fingerprint.ctimeSec, fingerprint.ctimeNsec = int64(stat.Ctim.Sec), int64(stat.Ctim.Nsec)
	}
	return fingerprint, nil
}
