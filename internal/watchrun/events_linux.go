package watchrun

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const observationBackup = 10 * time.Second

// eventWaiter owns all descriptors for one observer. Events are hints only:
// callers always rescan records and validate process identity after waking.
type eventWaiter struct {
	inotify, pidfd int
	poller         *os.File
	stopCancel     func() bool
	cancelDone     chan struct{}
	backup         time.Duration
	nextBackup     time.Time
}

func newEventWaiter(ctx context.Context, dir string, id Identity, bootID string, backup time.Duration, events bool) *eventWaiter {
	w := &eventWaiter{inotify: -1, pidfd: -1, backup: backup, nextBackup: time.Now().Add(backup)}
	if !events {
		return w
	}
	fd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return w
	}
	// os.NewFile registers nonblocking descriptors with Go's runtime poller.
	// Waiting on the epoll descriptor thus parks a goroutine, not an OS thread.
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return w
	}
	w.poller = os.NewFile(uintptr(fd), "watchrun-events")
	if err := w.poller.SetReadDeadline(time.Time{}); err != nil {
		_ = w.poller.Close()
		w.poller = nil
		return w
	}
	w.cancelDone = make(chan struct{})
	w.stopCancel = context.AfterFunc(ctx, func() {
		defer close(w.cancelDone)
		_ = w.poller.SetReadDeadline(time.Now())
	})
	if fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK); err == nil {
		// Do not watch access/open/close-nowrite: observation itself generates
		// those events. Watch the directory to catch atomic completion renames
		// and logs created after setup, without touching the records.
		mask := uint32(unix.IN_CREATE | unix.IN_MODIFY | unix.IN_CLOSE_WRITE | unix.IN_MOVED_TO | unix.IN_MOVED_FROM | unix.IN_DELETE | unix.IN_ATTRIB | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF)
		if _, err := unix.InotifyAddWatch(fd, dir, mask); err == nil {
			w.inotify = fd
		} else {
			_ = unix.Close(fd)
		}
	}
	if id.Boot == bootID {
		if fd, err := unix.PidfdOpen(id.PID, 0); err == nil {
			// Validate AFTER opening: a numeric PID may already refer to a
			// replacement. Never infer wrapper death from an unvalidated pidfd.
			start, _, err := processIdentity(id.PID)
			if err == nil && start == id.Start {
				w.pidfd = fd
			} else {
				_ = unix.Close(fd)
			}
		}
	}
	for _, source := range []*int{&w.inotify, &w.pidfd} {
		if *source >= 0 {
			if err := unix.EpollCtl(fd, unix.EPOLL_CTL_ADD, *source, &unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(*source)}); err != nil {
				w.disable(source)
			}
		}
	}
	return w
}

func (w *eventWaiter) close() {
	if w.stopCancel != nil && !w.stopCancel() {
		// Join a running callback before closing its poll descriptor.
		<-w.cancelDone
	}
	for _, fd := range []int{w.inotify, w.pidfd} {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}
	if w.poller != nil {
		_ = w.poller.Close()
	}
}

func (w *eventWaiter) disable(fd *int) {
	if *fd >= 0 {
		_ = unix.Close(*fd)
		*fd = -1
	}
}

// drainEvents is bounded even under a hot producer. Queue overflow is a wakeup
// like any other event: the subsequent full scan recovers authoritative state.
func (w *eventWaiter) drainEvents() {
	var buf [64 << 10]byte
	for pass := 0; pass < 16; pass++ {
		n, err := unix.Read(w.inotify, buf[:])
		if err == unix.EAGAIN {
			return
		}
		if err != nil || n == 0 {
			w.disable(&w.inotify)
			return
		}
		for offset := 0; offset+unix.SizeofInotifyEvent <= n; {
			mask := binary.NativeEndian.Uint32(buf[offset+4:])
			size := int(binary.NativeEndian.Uint32(buf[offset+12:]))
			if size > n-offset-unix.SizeofInotifyEvent {
				w.disable(&w.inotify)
				return
			}
			if mask&(unix.IN_IGNORED|unix.IN_DELETE_SELF|unix.IN_MOVE_SELF|unix.IN_UNMOUNT) != 0 {
				// A removed watch cannot recover on its own; leaving it polled
				// can repeatedly wake on terminal events on some kernels.
				w.disable(&w.inotify)
				return
			}
			offset += unix.SizeofInotifyEvent + size
		}
	}
}

func (w *eventWaiter) wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	remaining := time.Until(w.nextBackup)
	if remaining <= 0 {
		w.nextBackup = time.Now().Add(w.backup)
		return nil
	}
	if w.inotify < 0 && w.pidfd < 0 {
		return w.waitBackup(ctx)
	}
	if err := w.poller.SetReadDeadline(w.nextBackup); err != nil {
		w.disable(&w.inotify)
		w.disable(&w.pidfd)
		return w.waitBackup(ctx)
	}
	// Cancellation may have set an expired deadline just before the reset.
	if err := ctx.Err(); err != nil {
		return err
	}
	var events [2]unix.EpollEvent
	var n int
	var pollErr error
	raw, err := w.poller.SyscallConn()
	if err == nil {
		err = raw.Read(func(fd uintptr) bool {
			n, pollErr = unix.EpollWait(int(fd), events[:], 0)
			return n > 0 || pollErr != nil
		})
	}
	if err == nil {
		err = pollErr
	}
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			w.nextBackup = time.Now().Add(w.backup)
			return ctx.Err()
		}
		if err == unix.EINTR {
			return ctx.Err()
		}
		// Restricted kernels/sandboxes may reject polling or the APIs.
		// Disable failed event sources rather than repeatedly retrying them.
		w.disable(&w.inotify)
		w.disable(&w.pidfd)
		return w.waitBackup(ctx)
	}
	for _, event := range events[:n] {
		if int(event.Fd) == w.inotify {
			if event.Events&unix.EPOLLIN != 0 {
				w.drainEvents()
			}
			if event.Events&(unix.EPOLLERR|unix.EPOLLHUP) != 0 {
				w.disable(&w.inotify)
			}
		} else if int(event.Fd) == w.pidfd {
			// Death readiness is persistent. Consume it once by closing our
			// descriptor; processIdentity, not readiness, decides the outcome.
			w.disable(&w.pidfd)
		}
	}
	return ctx.Err()
}

func (w *eventWaiter) waitBackup(ctx context.Context) error {
	timer := time.NewTimer(time.Until(w.nextBackup))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		w.nextBackup = time.Now().Add(w.backup)
		return nil
	}
}
