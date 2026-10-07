// Package privatefile provides shared private-directory and durable atomic-file
// operations without depending on conversation history or authentication policy.
package privatefile

import (
	"fmt"
	"os"
	"path/filepath"
)

// PrivateDir creates a directory with mode 0700 and rejects an existing final
// symlink, non-directory or unsafe mode. Ancestor policy belongs to the caller.
func PrivateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return fmt.Errorf("unsafe private directory %s", path)
	}
	return nil
}

// AtomicFile writes and fsyncs bytes, then renames within the destination directory
// and fsyncs that directory. The parent must exist; mode applies to the new file.
// Replacing a symlink replaces its directory entry, never its target. Filesystem
// calls cannot be interrupted by a context; callers check cancellation beforehand.
func AtomicFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".ttc-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err = file.Chmod(mode); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temp, path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// SyncDir fsyncs a directory so entry updates reach durable storage.
func SyncDir(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
