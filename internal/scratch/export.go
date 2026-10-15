package scratch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"ttc/internal/prompts"
)

// WriteExport exclusively creates path beneath the verified per-UID scratch
// directory and streams write into it. Existing targets and symlink components
// are rejected; parents must already exist. On failure it removes the partial
// file unless its directory entry no longer identifies the file it created.
// The callback must honor ctx during work that does not call its writer.
// Creation is atomic, not publication: readers may observe partial content while
// the callback runs. A successful return means writing and closing have finished.
func WriteExport(ctx context.Context, path string, write func(io.Writer) error) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	root, err := Verify()
	if err != nil {
		return 0, err
	}
	return writeExportAt(ctx, root, path, write)
}

func writeExportAt(ctx context.Context, root, path string, write func(io.Writer) error) (written int64, err error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !filepath.IsAbs(path) {
		return 0, errors.New(prompts.ScratchExportAbsolute)
	}
	for _, part := range strings.Split(path, string(os.PathSeparator)) {
		if part == ".." {
			return 0, errors.New(prompts.ScratchExportTraversal)
		}
	}
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return 0, errors.New(prompts.ScratchExportOutside)
	}
	rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	defer unix.Close(rootFD)
	parentFD, err := exportParentFD(rootFD, filepath.Dir(rel))
	if err != nil {
		return 0, err
	}
	defer unix.Close(parentFD)
	name := filepath.Base(rel)
	fd, err := unix.Openat(parentFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return 0, err
	}
	f := os.NewFile(uintptr(fd), path)
	var created unix.Stat_t
	if err = unix.Fstat(fd, &created); err != nil {
		// Without identity evidence cleanup cannot safely unlink this name.
		return 0, errors.Join(err, f.Close())
	}
	defer func() {
		err = errors.Join(err, f.Close())
		if err != nil {
			var current unix.Stat_t
			statErr := unix.Fstatat(parentFD, name, &current, unix.AT_SYMLINK_NOFOLLOW)
			if statErr != nil && !errors.Is(statErr, unix.ENOENT) {
				err = errors.Join(err, statErr)
			} else if statErr == nil && current.Dev == created.Dev && current.Ino == created.Ino {
				if removeErr := unix.Unlinkat(parentFD, name, 0); removeErr != nil && !errors.Is(removeErr, unix.ENOENT) {
					err = errors.Join(err, removeErr)
				}
			}
		}
	}()
	// Open applies the process umask; export permissions are exactly private 0600.
	if err = f.Chmod(0600); err != nil {
		return 0, err
	}
	w := &exportWriter{ctx: ctx, file: f}
	err = write(w)
	written = w.written
	if err == nil {
		err = ctx.Err()
	}
	return written, err
}

type exportWriter struct {
	ctx     context.Context
	file    *os.File
	written int64
}

// exportParentFD walks an already validated local directory path without
// following symlinks. Each pinned directory owns the next component lookup.
func exportParentFD(rootFD int, relative string) (int, error) {
	flags := unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(rootFD, ".", flags, 0)
	if err != nil {
		return -1, err
	}
	if relative == "." {
		return fd, nil
	}
	for _, part := range strings.Split(relative, string(os.PathSeparator)) {
		next, err := unix.Openat(fd, part, flags, 0)
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

func (w *exportWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) != 0 {
		if err := w.ctx.Err(); err != nil {
			return written, err
		}
		n := min(len(p), 64<<10)
		count, err := w.file.Write(p[:n])
		written += count
		w.written += int64(count)
		p = p[count:]
		if err != nil {
			return written, err
		}
		if count != n {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}
