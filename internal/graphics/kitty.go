// Package graphics implements bounded Kitty Unicode image placements over SSH/tmux.
package graphics

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"image"
	"io"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
)

// Kitty owns transmitted images for one frontend. Only its event loop calls it.
// Begin/End retain only assets used by the current viewport; Close frees owned IDs.
type Kitty struct {
	Writer io.Writer
	Tmux   bool
	next   uint32
	images map[string]uint32
	used   map[string]bool
}

// New creates an image sender with randomized IDs to avoid other TUI collisions.
func New(w io.Writer, tmux bool) *Kitty {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return &Kitty{Writer: w, Tmux: tmux, next: binary.LittleEndian.Uint32(b[:]) | 0x1000000, images: map[string]uint32{}}
}

// Begin starts a frame, marking previously sent images unused until placed.
func (k *Kitty) Begin() { k.used = map[string]bool{} }
func (k *Kitty) format(s string) string {
	if k.Tmux {
		return "\x1bPtmux;" + strings.ReplaceAll(s, "\x1b", "\x1b\x1b") + "\x1b\\"
	}
	return s
}

// Place sends PNG bytes once for an asset/geometry, then returns self-contained
// placeholder rows. Each cell carries explicit row, column and all image ID bits.
// Grid axes are terminal columns/rows, limited to the diacritic table (297).
func (k *Kitty) Place(key string, m image.Image, columns, rows int) ([]string, error) {
	if columns < 1 || rows < 1 || columns > 297 || rows > 297 {
		return nil, fmt.Errorf("invalid image grid %dx%d", columns, rows)
	}
	placement := fmt.Sprintf("%s:%dx%d", key, columns, rows)
	id, ok := k.images[placement]
	if !ok {
		hash := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", k.next, placement)))
		id = binary.LittleEndian.Uint32(hash[:4])
		if id == 0 {
			id = 1
		}
		opts := &kitty.Options{Action: kitty.TransmitAndPut, Quite: 2, ID: int(id), Format: kitty.PNG, Transmission: kitty.Direct, Chunk: true, ChunkFormatter: k.format, Columns: columns, Rows: rows, VirtualPlacement: true, DoNotMoveCursor: true}
		if err := kitty.EncodeGraphics(k.Writer, m, opts); err != nil {
			return nil, err
		}
		k.images[placement] = id
	}
	k.used[placement] = true
	return k.Rows(key, columns, rows)
}

// Rows returns placeholder cells without sending pixels. Layout uses this before
// the viewport is known; Place transmits only assets that intersect visible rows.
func (k *Kitty) Rows(key string, columns, rows int) ([]string, error) {
	if columns < 1 || rows < 1 || columns > 297 || rows > 297 {
		return nil, fmt.Errorf("invalid image grid")
	}
	key = fmt.Sprintf("%s:%dx%d", key, columns, rows)
	hash := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", k.next, key)))
	id := binary.LittleEndian.Uint32(hash[:4])
	if id == 0 {
		id = 1
	}
	out := make([]string, rows)
	for y := range rows {
		var b strings.Builder
		fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dm", (id>>16)&255, (id>>8)&255, id&255)
		for x := range columns {
			b.WriteRune(kitty.Placeholder)
			b.WriteRune(kitty.Diacritic(y))
			b.WriteRune(kitty.Diacritic(x))
			b.WriteRune(kitty.Diacritic(int(id >> 24)))
		}
		b.WriteString("\x1b[39m")
		out[y] = b.String()
	}
	return out, nil
}

// End frees images outside the viewport, bounding terminal memory by visible assets.
func (k *Kitty) End() error {
	for key, id := range k.images {
		if !k.used[key] {
			if _, err := io.WriteString(k.Writer, k.format(ansi.KittyGraphics(nil, "a=d", "d=I", fmt.Sprintf("i=%d", id), "q=2"))); err != nil {
				return err
			}
			delete(k.images, key)
		}
	}
	return nil
}

// Close frees only this frontend's images, leaving other terminal clients alone.
func (k *Kitty) Close() error { k.Begin(); return k.End() }
