package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"ttc/internal/tool"
)

// setupExo is a standalone operator workflow: credentials never enter history.
func setupExo(ctx context.Context, path string, input io.Reader, output io.Writer, client *http.Client) error {
	original, err := setupConfigInfo(path)
	if err != nil {
		return err
	}
	config, err := loadWebSearchConfig(path)
	if err != nil {
		return err
	}
	reader := input
	if file, ok := input.(*os.File); ok {
		reader = setupInputReader{ctx: ctx, file: file}
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 8192)
	readLine := func() (string, error) {
		if scanner.Scan() {
			return scanner.Text(), nil
		}
		if scanner.Err() != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", errors.New("could not read setup input (maximum line length is 8191 bytes)")
		}
		return "", io.EOF
	}
	fmt.Fprint(output, "Exa API key: ")
	var key string
	if file, ok := input.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		state, stateErr := unix.IoctlGetTermios(int(file.Fd()), unix.TCGETS)
		if stateErr != nil {
			return errors.New("could not inspect terminal settings")
		}
		hidden := *state
		hidden.Lflag &^= unix.ECHO | unix.ECHONL
		if err = unix.IoctlSetTermios(int(file.Fd()), unix.TCSETS, &hidden); err != nil {
			return errors.New("could not disable terminal echo")
		}
		key, err = readLine()
		restoreErr := unix.IoctlSetTermios(int(file.Fd()), unix.TCSETS, state)
		fmt.Fprintln(output)
		if restoreErr != nil {
			return errors.New("could not restore terminal settings")
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("could not read Exa API key from terminal")
		}
	} else {
		key, err = readLine()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("could not read Exa API key from stdin")
		}
	}
	config.APIKey = key
	if key == "" {
		return errors.New("Exa API key must not be empty")
	}
	if err = config.Validate(); err != nil {
		return fmt.Errorf("Exa configuration: %w", err)
	}
	if original != nil {
		fmt.Fprint(output, "Web-search config already exists. Overwrite its API key? [y/N]: ")
		answer, readErr := readLine()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(output, "Setup cancelled; configuration unchanged.")
			return nil
		}
	}
	fmt.Fprintln(output, "Checking Exa key with a test search...")
	checkConfig := config
	if endpoint := os.Getenv("TTC_EXA_URL"); endpoint != "" {
		checkConfig.Endpoint = endpoint
	}
	if err = checkConfig.Validate(); err != nil {
		return fmt.Errorf("Exa verification configuration: %w", err)
	}
	for _, raw := range []string{config.Endpoint, checkConfig.Endpoint} {
		if raw == "" {
			continue
		}
		endpoint, _ := url.Parse(raw) // Both configs were validated above.
		host := endpoint.Hostname()
		ip := net.ParseIP(host)
		if endpoint.Scheme == "http" && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("Exa key verification requires HTTPS (HTTP is allowed only for localhost or loopback addresses); configuration unchanged")
		}
	}
	if err = tool.CheckWebSearch(ctx, client, checkConfig); err != nil {
		return fmt.Errorf("Exa key verification failed; configuration unchanged: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = saveSetupConfig(path, original, config); err != nil {
		return err
	}
	fmt.Fprintln(output, "Exa key verified and saved. Restart TTC to use it. Inside rail, recreate rail from the host.")
	return nil
}

// Polling owns no goroutine and keeps idle terminal/pipe reads cancellable.
// Setup is the sole reader of this descriptor; canonical terminal input becomes
// readable only after a line is complete, while echo is disabled above.
type setupInputReader struct {
	ctx  context.Context
	file *os.File
}

func (r setupInputReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		fds := []unix.PollFd{{Fd: int32(r.file.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 100)
		if errors.Is(err, unix.EINTR) || n == 0 && err == nil {
			continue
		}
		if err != nil {
			return 0, err
		}
		if fds[0].Revents&unix.POLLNVAL != 0 {
			return 0, os.ErrClosed
		}
		n, err = unix.Read(int(r.file.Fd()), p)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 && err == nil {
			return 0, io.EOF
		}
		return n, err
	}
}

func setupConfigInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect web search config: %w", err)
	}
	if !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		return nil, errors.New("setup requires a regular, singly linked, nonsymlink config file")
	}
	return info, nil
}

// saveSetupConfig stages a private file in the same directory. Failed validation
// never truncates existing configuration; new targets are created without clobbering.
func saveSetupConfig(path string, original os.FileInfo, config tool.WebSearchConfig) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create web search config directory: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".web-search-*")
	if err != nil {
		return fmt.Errorf("create private web search config: %w", err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write web search config: %w", err)
	}
	if err = file.Sync(); err != nil {
		return fmt.Errorf("sync web search config: %w", err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close web search config: %w", err)
	}
	if original == nil {
		err = os.Link(file.Name(), path)
	} else {
		// Best-effort detection of intervening edits. Concurrent external writers
		// are not serialized with the final rename; do not run setups concurrently.
		current, statErr := setupConfigInfo(path)
		if statErr != nil {
			return statErr
		}
		if current == nil || !os.SameFile(original, current) || current.Size() != original.Size() || !current.ModTime().Equal(original.ModTime()) {
			return errors.New("web search config changed during setup; retry")
		}
		err = os.Rename(file.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("save web search config (inside rail, run setup from the host): %w", err)
	}
	return nil
}
