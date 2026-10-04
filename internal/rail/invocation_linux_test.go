package rail

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestInvocationAnonymousFilesExactSealedAndClosed(t *testing.T) {
	v := invocation{executable: "/bin/true", environment: []string{}, files: []string{"exact\x00\xffbytes", ""}}
	cmd, cleanup, err := v.prepareCommand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	files := append([]*os.File(nil), cmd.ExtraFiles...)
	if len(files) != len(v.files) {
		t.Fatal("payload descriptors missing")
	}
	for i, file := range files {
		data, err := io.ReadAll(file)
		if err != nil || string(data) != v.files[i] {
			t.Fatalf("payload %d = %q, %v", i, data, err)
		}
		seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
		want := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
		if err != nil || seals != want {
			t.Fatalf("unsealed payload: %d, %v", seals, err)
		}
		flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC == 0 {
			t.Fatalf("payload leaks into unrelated subprocesses: %d, %v", flags, err)
		}
		if _, err := file.WriteString("changed"); err == nil {
			t.Fatal("payload descriptor remained writable")
		}
	}
	cleanup()
	cleanup() // The deferred and post-Start cleanup paths can both run.
	if len(cmd.ExtraFiles) != 0 {
		t.Fatal("command retained closed descriptors")
	}
	for _, file := range files {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("payload descriptor remains open: %v", err)
		}
	}
}

func TestInvocationAnonymousFilesCancellationAndStartFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v := invocation{executable: "/does-not-exist", files: []string{"payload"}}
	if cmd, cleanup, err := v.prepareCommand(ctx); !errors.Is(err, context.Canceled) || cmd != nil || cleanup != nil {
		t.Fatalf("cancelled preparation = %v, %v", cmd, err)
	}
	cmd, cleanup, err := v.prepareCommand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	file := cmd.ExtraFiles[0]
	if err := cmd.Start(); err == nil {
		t.Fatal("missing command started")
	}
	cleanup()
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Start failure leaked descriptor: %v", err)
	}
}

func TestInvocationAnonymousFilesCloseAfterSuccessfulStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	v := invocation{executable: "/bin/sh", args: []string{"-c", "IFS= read -r value; test \"$value\" = done"}, files: []string{"payload"}}
	cmd, cleanup, err := v.prepareCommand(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	file := cmd.ExtraFiles[0]
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	cleanup() // The child is still blocked on stdin, not yet exiting.
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) || len(cmd.ExtraFiles) != 0 {
		t.Fatalf("successful Start retained parent payload handle: %v", err)
	}
	if _, err := io.WriteString(stdin, "done\n"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}
