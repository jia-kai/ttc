package rail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const clientServerCommand = "__rail_clients"
const maxClientMessage = 64 << 10

type clientRequest struct {
	Operation string   `json:"operation"`
	Env       []string `json:"env,omitempty"`
}

type clientResponse struct {
	Sessions []string `json:"sessions,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// IsClientServer identifies the private in-sandbox tmux client service.
func IsClientServer(args []string) bool {
	return len(args) == 1 && args[0] == clientServerCommand
}

// ServeClients owns the foreground tmux server and fixed list/attach service
// inside the sandbox. The service starts independently of tmux configuration:
// tmux -D loads its configuration only after the first native client connects.
// Native tmux clients must never run on the host: detach-client -E can execute
// commands in the client, and an untrusted peer can send execution messages.
func ServeClients(ctx context.Context) error {
	return serveTmux(ctx, "/run/ttc-rail/control.sock", "/run/ttc-rail/server.sock", "/run/ttc-rail/tmux.conf")
}

func serveTmux(ctx context.Context, control, socket, config string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", "-D", "-S", socket, "-f", config)
	cmd.Env = clientEnvironment()
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start tmux server: %w", err)
	}
	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		cancel() // Last-session exit also closes the service and attached clients.
		exited <- err
	}()
	err := serveClients(ctx, control, socket)
	cancel()
	waitErr := <-exited
	if err != nil {
		return err
	}
	return waitErr
}

func serveClients(ctx context.Context, control, tmuxSocket string) error {
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: control, Net: "unixpacket"})
	if err != nil {
		return err
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(ctx)
	go func() { <-ctx.Done(); listener.Close() }()
	var handlers sync.WaitGroup
	defer func() {
		cancel()
		handlers.Wait()
	}()
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			cancel()
			return err
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer conn.Close()
			handleClient(ctx, conn, tmuxSocket)
		}()
	}
}

func receiveClient(conn *net.UnixConn) (clientRequest, []*os.File, error) {
	var req clientRequest
	data := make([]byte, maxClientMessage)
	oob := make([]byte, unix.CmsgSpace(4))
	raw, err := conn.SyscallConn()
	if err != nil {
		return req, nil, err
	}
	var n, oobn, flags int
	var readErr error
	err = raw.Read(func(fd uintptr) bool {
		// CLOEXEC must be atomic with receipt: another handler may fork a tmux
		// client before a subsequent CloseOnExec call would protect these FDs.
		n, oobn, flags, _, readErr = unix.Recvmsg(int(fd), data, oob, unix.MSG_CMSG_CLOEXEC)
		return !errors.Is(readErr, unix.EAGAIN) && !errors.Is(readErr, unix.EINTR)
	})
	if err != nil {
		return req, nil, err
	}
	if readErr != nil {
		return req, nil, readErr
	}
	messages, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return req, nil, err
	}
	var files []*os.File
	for _, message := range messages {
		fds, err := unix.ParseUnixRights(&message)
		if err != nil {
			for _, file := range files {
				file.Close()
			}
			return req, nil, err
		}
		for _, fd := range fds {
			files = append(files, os.NewFile(uintptr(fd), "rail-terminal"))
		}
	}
	if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
		err = errors.New("rail client request is too large")
	} else {
		err = json.Unmarshal(data[:n], &req)
	}
	if err != nil {
		for _, file := range files {
			file.Close()
		}
		return req, nil, err
	}
	return req, files, nil
}

func handleClient(ctx context.Context, conn *net.UnixConn, tmuxSocket string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Even an idle/malformed peer cannot keep shutdown waiting indefinitely.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	req, files, err := receiveClient(conn)
	for _, file := range files {
		defer file.Close()
	}
	conn.SetReadDeadline(time.Time{})
	response := clientResponse{}
	if err != nil {
		response.Error = err.Error()
	} else if req.Operation == "list" && len(files) == 0 {
		response.Sessions, err = localSessions(ctx, tmuxSocket)
	} else if req.Operation == "attach" && len(files) == 1 && term.IsTerminal(int(files[0].Fd())) {
		cmd := exec.CommandContext(ctx, "tmux", "-N", "-S", tmuxSocket, "attach-session")
		cmd.Env = insideClientEnvironment(req.Env)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = files[0], files[0], files[0]
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 2 * time.Second
		if err = cmd.Start(); err == nil {
			readerDone := make(chan struct{})
			clientDone := make(chan struct{})
			go func() {
				defer close(readerDone)
				for {
					update, descriptors, readErr := receiveClient(conn)
					for _, file := range descriptors {
						file.Close()
					}
					if readErr != nil || update.Operation != "resize" || len(descriptors) != 0 {
						select {
						case <-clientDone:
							return
						default:
						}
						cancel()
						return
					}
					_ = cmd.Process.Signal(syscall.SIGWINCH)
				}
			}()
			err = cmd.Wait()
			close(clientDone)
			// Wake the input watcher without closing the response side.
			conn.SetReadDeadline(time.Now())
			<-readerDone
			conn.SetReadDeadline(time.Time{})
		}
	} else {
		err = errors.New("invalid rail client operation or terminal descriptor")
	}
	if err != nil {
		response.Error = err.Error()
	}
	data, marshalErr := json.Marshal(response)
	if marshalErr == nil && len(data) <= maxClientMessage {
		conn.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = conn.Write(data)
	}
}

func localSessions(ctx context.Context, socket string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", "-N", "-S", socket, "list-sessions", "-F", "#{session_name}\t#{session_windows}\t#{session_attached}")
	cmd.Env = clientEnvironment()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, errors.New("tmux has no sessions")
	}
	return lines, nil
}

// Instance paths stay launch-time values; terminal/display settings may change
// between attachments. Preserve the shared data root and forwarded agent socket;
// tmux's update-environment must not replace them with host attachment paths.
func insideClientEnvironment(requested []string) []string {
	fixed := map[string]bool{}
	for _, key := range []string{"HOME", "SHELL", "PATH", "TTC_DATA_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "SSH_AUTH_SOCK", "TMUX", "TMUX_PANE"} {
		fixed[key] = true
	}
	var env []string
	for _, item := range requested {
		key, _, _ := strings.Cut(item, "=")
		if !fixed[key] {
			env = append(env, item)
		}
	}
	for key := range fixed {
		if key != "TMUX" && key != "TMUX_PANE" {
			if value, ok := os.LookupEnv(key); ok {
				env = append(env, key+"="+value)
			}
		}
	}
	return env
}

func clientConnection(ctx context.Context, socket string, req clientRequest, terminal *os.File) (*net.UnixConn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unixpacket", socket)
	if err != nil {
		return nil, err
	}
	unixConn := conn.(*net.UnixConn)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	data, err := json.Marshal(req)
	if err != nil || len(data) > maxClientMessage {
		conn.Close()
		return nil, errors.New("rail client environment is too large")
	}
	var rights []byte
	if terminal != nil {
		rights = unix.UnixRights(int(terminal.Fd()))
	}
	if _, _, err = unixConn.WriteMsgUnix(data, rights, nil); err != nil {
		conn.Close()
		return nil, err
	}
	return unixConn, nil
}

func receiveResponse(conn *net.UnixConn) (clientResponse, error) {
	var response clientResponse
	data := make([]byte, maxClientMessage)
	n, _, flags, _, err := conn.ReadMsgUnix(data, nil)
	if err != nil {
		return response, err
	}
	if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
		return response, errors.New("rail response is too large")
	}
	if err := json.Unmarshal(data[:n], &response); err != nil {
		return response, err
	}
	if response.Error != "" {
		return response, errors.New(response.Error)
	}
	return response, nil
}

func sessions(ctx context.Context, control string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := clientConnection(ctx, control, clientRequest{Operation: "list"}, nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	conn.SetDeadline(deadline)
	response, err := receiveResponse(conn)
	return response.Sessions, err
}

func attachClient(ctx context.Context, control string, terminal *os.File) error {
	state, err := term.GetState(int(terminal.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(terminal.Fd()), state)
	conn, err := clientConnection(ctx, control, clientRequest{Operation: "attach", Env: clientEnvironment()}, terminal)
	if err != nil {
		return err
	}
	defer conn.Close()
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	defer signal.Stop(resize)
	return waitForClient(ctx, conn, resize)
}

func waitForClient(ctx context.Context, conn *net.UnixConn, resize <-chan os.Signal) error {
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	result := make(chan error, 1)
	go func() { _, err := receiveResponse(conn); result <- err }()
	for {
		select {
		case err := <-result:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-resize:
			if _, err := conn.Write([]byte(`{"operation":"resize"}`)); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
		}
	}
}
