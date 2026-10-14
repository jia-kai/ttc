// Package filelock coordinates short operations on shared private files.
package filelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"ttc/internal/prompts"
)

// Acquire waits cancelably for an exclusive advisory lock on a private regular
// file. Closing the returned file releases the lock; never replace its inode.
func Acquire(ctx context.Context, path string) (*os.File, error) {
	return acquire(ctx, path, unix.LOCK_EX)
}

// AcquireShared waits cancelably for a shared advisory lock. Multiple readers
// may hold it until Close; exclusive Acquire waits until every reader closes.
func AcquireShared(ctx context.Context, path string) (*os.File, error) {
	return acquire(ctx, path, unix.LOCK_SH)
}

func acquire(ctx context.Context, path string, mode int) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fmt.Errorf(prompts.FileLockOpen, err)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		f.Close()
		return nil, errors.New(prompts.FileLockPrivateRegular)
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		err = unix.Flock(int(f.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			f.Close()
			return nil, fmt.Errorf(prompts.FileLockAcquire, err)
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}
