package rail

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestForegroundTmuxBootstrapsThroughClientService(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is optional outside rail runtime")
	}
	for _, plugin := range []string{"", "run-shell /nonexistent/rail-plugin\n"} {
		t.Run(fmt.Sprint(plugin != ""), func(t *testing.T) {
			root, err := os.MkdirTemp("", "rail-bootstrap-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)
			control, socket := filepath.Join(root, "control.sock"), filepath.Join(root, "server.sock")
			config := filepath.Join(root, "tmux.conf")
			if err := os.WriteFile(config, []byte(plugin+"set -g default-shell /bin/sh\nnew-session -d -s rail 'sleep 60'\nset -g exit-empty on\n"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- serveTmux(ctx, control, socket, config) }()
			completed := false
			defer func() {
				cancel()
				if !completed {
					<-done
				}
			}()
			// Only fixed IPC connects before readiness, never a host tmux client.
			for {
				if lines, err := sessions(ctx, control); err == nil && len(lines) == 1 && strings.HasPrefix(lines[0], "rail\t") {
					break
				}
				if ctx.Err() != nil {
					t.Fatal("foreground tmux did not bootstrap through the client service")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if out, err := exec.Command("tmux", "-N", "-S", socket, "kill-session", "-t", "rail").CombinedOutput(); err != nil {
				t.Fatalf("close last session: %v: %s", err, out)
			}
			select {
			case err := <-done:
				completed = true
				if err != nil {
					t.Fatal("last-session exit", err)
				}
			case <-ctx.Done():
				t.Fatal("last-session exit did not stop server and client service")
			}
		})
	}
}

func TestClientServiceRejectsInvalidRequestsAndClosesIdlePeers(t *testing.T) {
	root, err := os.MkdirTemp("", "rail-clients-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	control := filepath.Join(root, "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveClients(ctx, control, filepath.Join(root, "server.sock")) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Lstat(control); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("client service did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	file, err := os.CreateTemp(root, "not-a-terminal")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, req := range []clientRequest{{Operation: "unknown"}, {Operation: "attach"}, {Operation: "list"}} {
		conn, err := clientConnection(ctx, control, req, file)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(time.Second))
		_, err = receiveResponse(conn)
		conn.Close()
		if err == nil || !strings.Contains(err.Error(), "invalid rail client") {
			t.Fatalf("invalid operation/fd accepted: %+v %v", req, err)
		}
	}
	idle, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: control, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle peer blocked service shutdown")
	}
}

func TestInsideClientEnvironmentPreservesInstancePaths(t *testing.T) {
	t.Setenv("TTC_DATA_DIR", "/instance/state")
	t.Setenv("HOME", "/instance/home")
	t.Setenv("PATH", "/instance/bin")
	env := strings.Join(insideClientEnvironment([]string{
		"TTC_DATA_DIR=/other/state", "HOME=/other/home", "PATH=/other/bin",
		"TMUX=host", "TMUX_PANE=%1", "TERM=terminal-fixture", "DISPLAY=display-fixture",
	}), "\n")
	for _, want := range []string{"TTC_DATA_DIR=/instance/state", "HOME=/instance/home", "PATH=/instance/bin", "TERM=terminal-fixture", "DISPLAY=display-fixture"} {
		if !strings.Contains(env, want) {
			t.Fatal("missing environment", want, env)
		}
	}
	for _, unwanted := range []string{"/other/", "TMUX=", "TMUX_PANE="} {
		if strings.Contains(env, unwanted) {
			t.Fatal("invalid environment override", env)
		}
	}
}

func TestEmptyClientPacketClosesReceivedRights(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pair[0])
	file := os.NewFile(uintptr(pair[1]), "receiver")
	conn, err := net.FileConn(file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fixture, err := os.CreateTemp(t.TempDir(), "descriptor")
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	count := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	before := count()
	rights := unix.UnixRights(int(fixture.Fd()))
	// Go Sendmsg/WriteMsg wrappers insert a byte when sending rights. Use the
	// syscall directly to exercise an actually empty SOCK_SEQPACKET record.
	header := unix.Msghdr{Control: &rights[0]}
	header.SetControllen(len(rights))
	_, _, errno := unix.Syscall(unix.SYS_SENDMSG, uintptr(pair[0]), uintptr(unsafe.Pointer(&header)), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	_, files, err := receiveClient(conn.(*net.UnixConn))
	if err == nil || len(files) != 0 {
		t.Fatalf("empty packet accepted: %v %v", files, err)
	}
	if after := count(); after != before {
		t.Fatalf("empty rights packet leaked FDs: before=%d after=%d", before, after)
	}
}

func TestResizeWriteCancellation(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pair[1]) // Peer intentionally never reads resize messages.
	if err := unix.SetsockoptInt(pair[0], unix.SOL_SOCKET, unix.SO_SNDBUF, 1024); err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(pair[0]), "blocked-writer")
	conn, err := net.FileConn(file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	resize := make(chan os.Signal, 4096)
	for range cap(resize) {
		resize <- syscall.SIGWINCH
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- waitForClient(ctx, conn.(*net.UnixConn), resize) }()
	deadline := time.Now().Add(time.Second)
	for len(resize) == cap(resize) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if len(resize) == cap(resize) || len(resize) == 0 {
		t.Fatal("did not fill the socket's resize queue")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked resize write ignored cancellation")
	}
}
