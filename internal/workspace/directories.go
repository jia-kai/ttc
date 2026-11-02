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

// createDirs records only successful mkdir calls, not directories merely absent
// during preflight. Existing directories (including concurrent creators) are not ours.
func createDirs(c *PathChange) error {
	dirs := missingDirs(c.Path)
	for i := len(dirs) - 1; i >= 0; i-- {
		path := dirs[i]
		err := os.Mkdir(path, 0755)
		created := err == nil
		if err != nil && !os.IsExist(err) {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf(prompts.WorkspaceParentNotDirectory, path)
		}
		if created {
			if c.DirIDs == nil {
				c.DirIDs = make(map[string]DirectoryID)
			}
			c.CreatedDirs = append(c.CreatedDirs, path)
			c.DirIDs[path] = directoryID(info)
		}
	}
	return nil
}

func directoryID(info os.FileInfo) DirectoryID {
	stat := info.Sys().(*syscall.Stat_t) // Linux is the supported host platform.
	return DirectoryID{Device: uint64(stat.Dev), Inode: stat.Ino}
}

// Restoration may remove a previously owned shared parent after its last file
// disappears. Failed mutation cleanup is restricted to that attempt's mkdirs.
func (m *Manager) cleanupDirs(changes []PathChange, includeOwnedParents bool) {
	ids := make(map[string]DirectoryID)
	for _, c := range changes {
		if !c.After.Exists {
			for _, path := range c.CreatedDirs {
				if id, ok := c.DirIDs[path]; ok {
					ids[path] = id
				}
			}
			if includeOwnedParents {
				// The creating file may have disappeared in an earlier branch
				// switch while another file kept its shared parent nonempty.
				for path := filepath.Dir(c.Path); ; path = filepath.Dir(path) {
					if id, ok := m.ownedDirs[path]; ok {
						ids[path] = id
					}
					if path == filepath.Dir(path) {
						break
					}
				}
			}
		}
	}
	if m.ownedDirs == nil {
		m.ownedDirs = make(map[string]DirectoryID)
	}
	// Prefer a current runtime identity over immutable historical identities,
	// regardless of which file supplied the directory in this restore batch.
	for path := range ids {
		if id, ok := m.ownedDirs[path]; ok {
			ids[path] = id
		}
	}
	dirs := make([]string, 0, len(ids))
	for path := range ids {
		dirs = append(dirs, path)
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, path := range dirs {
		removeCreatedDir(path, ids[path])
		info, err := os.Lstat(path)
		if err == nil && info.IsDir() && directoryID(info) == ids[path] {
			// Keep verified ownership when another file still needs this parent.
			m.ownedDirs[path] = ids[path]
		} else {
			delete(m.ownedDirs, path)
		}
	}
}

// Open each ancestor without following symlinks, then bind inspection and removal
// to the same parent descriptor. Removal is directory-only even if the final
// entry changes after inspection; external writers are not serialized by Manager.
func removeCreatedDir(path string, id DirectoryID) {
	if !filepath.IsAbs(path) {
		return
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	defer func() { _ = unix.Close(fd) }()
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Dir(path), "/"), "/") {
		if part == "" {
			continue
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return
		}
		_ = unix.Close(fd)
		fd = next
	}
	var stat unix.Stat_t
	name := filepath.Base(path)
	if err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || id != (DirectoryID{Device: uint64(stat.Dev), Inode: stat.Ino}) {
		return
	}
	_ = unix.Unlinkat(fd, name, unix.AT_REMOVEDIR)
}
