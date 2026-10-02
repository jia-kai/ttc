package graphics

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Probe detects Kitty before tcell owns input. In tmux it uses the detected
// client identity and effective passthrough setting, since a graphics query's
// reply may not reach the pane. Direct connections query the terminal instead.
// The raw descriptor is restored on return;
// unrelated input is returned for the frontend to deliver to its terminal reader.
// Metadata queries take at most two seconds; direct queries take 500 ms and
// retain at most 4 KiB. An error explains why optional graphics are disabled.
func Probe(ctx context.Context, tmux bool) (bool, []byte, error) {
	if tmux {
		err := probeTmux(ctx)
		return err == nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}
	fd, err := unix.Open("/dev/tty", unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, nil, fmt.Errorf("open terminal for graphics query: %w", err)
	}
	defer unix.Close(fd)
	state, err := term.MakeRaw(fd)
	if err != nil {
		return false, nil, fmt.Errorf("prepare terminal for graphics query: %w", err)
	}
	defer term.Restore(fd, state)
	var pngBytes bytes.Buffer
	_ = png.Encode(&pngBytes, image.NewNRGBA(image.Rect(0, 0, 1, 1)))
	seq := ansi.KittyGraphics([]byte(base64.StdEncoding.EncodeToString(pngBytes.Bytes())), "a=q", "i=31337", "f=100", "s=1", "v=1", "t=d")
	written, err := unix.Write(fd, []byte(seq))
	if err != nil {
		return false, nil, fmt.Errorf("send graphics query: %w", err)
	}
	if written != len(seq) {
		return false, nil, errors.New("incomplete graphics query write")
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	var reply []byte
	for time.Now().Before(deadline) && len(reply) < 4096 {
		if err := ctx.Err(); err != nil {
			return false, reply, err
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if _, err = unix.Poll(poll, 20); err != nil {
			if err == unix.EINTR {
				continue
			}
			return false, reply, fmt.Errorf("wait for graphics reply: %w", err)
		}
		if poll[0].Revents&unix.POLLIN == 0 {
			continue
		}
		var b [1024]byte
		n, err := unix.Read(fd, b[:min(len(b), 4096-len(reply))])
		if err != nil {
			if err == unix.EINTR || err == unix.EAGAIN {
				continue
			}
			return false, reply, fmt.Errorf("read graphics reply: %w", err)
		}
		if n == 0 {
			return false, reply, errors.New("terminal closed during graphics query")
		}
		reply = append(reply, b[:n]...)
		if supported, found, pending := probeReply(reply); found {
			if !supported {
				return false, pending, errors.New("terminal rejected the Kitty graphics query")
			}
			return true, pending, nil
		}
	}
	if len(reply) == 4096 {
		return false, reply, errors.New("graphics query input exceeds 4 KiB without a reply")
	}
	return false, reply, errors.New("terminal graphics query timed out after 500 ms")
}

// probeReply removes only a complete response to our query, preserving all other
// input, including replies to unrelated queries and keys arriving during startup.
func probeReply(input []byte) (bool, bool, []byte) {
	const prefix = "\x1b_G"
	for offset := 0; offset < len(input); {
		start := bytes.Index(input[offset:], []byte(prefix))
		if start < 0 {
			break
		}
		start += offset
		end := bytes.Index(input[start+len(prefix):], []byte("\x1b\\"))
		if end < 0 {
			break
		}
		end += start + len(prefix)
		body := string(input[start+len(prefix) : end])
		header, result, ok := strings.Cut(body, ";")
		ours := false
		for _, field := range strings.Split(header, ",") {
			if field == "i=31337" {
				ours = true
			}
		}
		if ok && ours {
			pending := append([]byte(nil), input[:start]...)
			pending = append(pending, input[end+2:]...)
			return result == "OK", true, pending
		}
		offset = end + 2
	}
	return false, false, input
}

// Geometry computes a capped image grid using measured cell pixel dimensions.
func Geometry(m image.Image, cellWidth, cellHeight, maxColumns, maxRows int) (int, int) {
	if cellWidth < 1 {
		cellWidth = 8
	}
	if cellHeight < 1 {
		cellHeight = 16
	}
	b := m.Bounds()
	scale := min(float64(max(1, maxColumns)*cellWidth)/float64(b.Dx()), float64(max(1, maxRows)*cellHeight)/float64(b.Dy()), 1)
	cols := max(1, int(float64(b.Dx())*scale/float64(cellWidth)+0.999))
	rows := max(1, int(float64(b.Dy())*scale/float64(cellHeight)+0.999))
	return min(cols, max(1, maxColumns)), min(rows, max(1, maxRows))
}
