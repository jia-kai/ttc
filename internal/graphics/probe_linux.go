package graphics

import (
	"bytes"
	"image"
	"image/png"
	"strings"
	"time"

	"encoding/base64"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Probe queries the underlying terminal before tcell owns input. Terminal names
// are not reliable through SSH/tmux. The raw descriptor is restored on return;
// unrelated input is returned for the frontend to deliver to its terminal reader.
// The query is bounded to 500 ms and 4 KiB. tmux must permit DCS passthrough.
func Probe(tmux bool) (bool, []byte) {
	fd, err := unix.Open("/dev/tty", unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, nil
	}
	defer unix.Close(fd)
	state, err := term.MakeRaw(fd)
	if err != nil {
		return false, nil
	}
	defer term.Restore(fd, state)
	var pngBytes bytes.Buffer
	_ = png.Encode(&pngBytes, image.NewNRGBA(image.Rect(0, 0, 1, 1)))
	seq := ansi.KittyGraphics([]byte(base64.StdEncoding.EncodeToString(pngBytes.Bytes())), "a=q", "i=31337", "f=100", "s=1", "v=1", "t=d")
	if tmux {
		seq = "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
	}
	if _, err = unix.Write(fd, []byte(seq)); err != nil {
		return false, nil
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	var reply []byte
	for time.Now().Before(deadline) && len(reply) < 4096 {
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if _, err = unix.Poll(poll, 20); err != nil {
			if err == unix.EINTR {
				continue
			}
			break
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
			break
		}
		reply = append(reply, b[:n]...)
		if supported, found, pending := probeReply(reply); found {
			return supported, pending
		}
	}
	return false, reply
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
