package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"ttc/internal/prompts"
)

// directoryIdentity identifies a directory observed after a successful mkdir.
type directoryIdentity struct {
	device uint64
	inode  uint64
}

// createdDirectory is a runtime mkdir receipt, not a claim recovered from history.
type createdDirectory struct {
	path string
	id   directoryIdentity
}

// directoryOwnership owns cleanup authority for one Manager lifetime, under its
// mutation lock. Only successful mkdir receipts grant authority. Historical file
// changes and directory existence never do.
type directoryOwnership struct {
	owned map[string]directoryIdentity
}

// finish registers an attempt's mkdirs even when its file write failed. Cleanup
// of an unapplied attempt is limited to its receipts, not earlier owned parents.
func (o *directoryOwnership) finish(effect mutationEffect) {
	if len(effect.created) == 0 {
		return
	}
	if o.owned == nil {
		o.owned = make(map[string]directoryIdentity)
	}
	for _, dir := range effect.created {
		o.owned[dir.path] = dir.id
	}
	if !effect.applied {
		o.cleanup(effect.created)
	}
}

// removedFiles cleans owned ancestors only after a completed restore. Parents
// kept nonempty by other files remain owned, independent of the creating file's
// history or presence on the selected branch.
func (o *directoryOwnership) removedFiles(paths []string) {
	candidates := make(map[string]directoryIdentity)
	for _, file := range paths {
		for path := filepath.Dir(file); ; path = filepath.Dir(path) {
			if id, ok := o.owned[path]; ok {
				candidates[path] = id
			}
			if path == filepath.Dir(path) {
				break
			}
		}
	}
	dirs := make([]createdDirectory, 0, len(candidates))
	for path, id := range candidates {
		dirs = append(dirs, createdDirectory{path, id})
	}
	o.cleanup(dirs)
}

func (o *directoryOwnership) cleanup(dirs []createdDirectory) {
	// Children must be removed before their parents. Copy so sorting does not
	// mutate the attempt's receipt.
	dirs = append([]createdDirectory(nil), dirs...)
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].path) > len(dirs[j].path) })
	for _, dir := range dirs {
		if !removeCreatedDir(dir.path, dir.id) {
			delete(o.owned, dir.path)
		}
	}
}

// createDirs returns only successful mkdir calls, including partial progress on
// failure. Existing directories (including concurrent creators) are not ours.
func createDirs(path string) ([]createdDirectory, error) {
	var created []createdDirectory
	dirs := missingDirs(path)
	for i := len(dirs) - 1; i >= 0; i-- {
		path := dirs[i]
		err := os.Mkdir(path, 0755)
		ours := err == nil
		if err != nil && !os.IsExist(err) {
			return created, err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return created, err
		}
		if !info.IsDir() {
			return created, fmt.Errorf(prompts.WorkspaceParentNotDirectory, path)
		}
		if ours {
			created = append(created, createdDirectory{path, directoryID(info)})
		}
	}
	return created, nil
}

func directoryID(info os.FileInfo) directoryIdentity {
	stat := info.Sys().(*syscall.Stat_t) // Linux is the supported host platform.
	return directoryIdentity{device: uint64(stat.Dev), inode: stat.Ino}
}

// Open each ancestor without following symlinks, then bind inspection and removal
// to the same parent descriptor. Removal is directory-only even if the final
// entry changes after inspection; external writers are not serialized by Manager.
// The result retains authority only if the same directory verifiably remains.
func removeCreatedDir(path string, id directoryIdentity) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer func() { _ = unix.Close(fd) }()
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Dir(path), "/"), "/") {
		if part == "" {
			continue
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return false
		}
		_ = unix.Close(fd)
		fd = next
	}
	name := filepath.Base(path)
	matches := func() bool {
		var stat unix.Stat_t
		return unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW) == nil &&
			stat.Mode&unix.S_IFMT == unix.S_IFDIR && id == (directoryIdentity{device: uint64(stat.Dev), inode: stat.Ino})
	}
	if !matches() {
		return false
	}
	if unix.Unlinkat(fd, name, unix.AT_REMOVEDIR) == nil {
		return false
	}
	return matches()
}
