package tui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"ttc/internal/render"
)

// pendingPreviewBytes caps source work per visible row, independently of the
// composer size. Previewing never changes original inputs or their snapshots.
const pendingPreviewBytes = 4096

// pendingInputPreview normalizes only a bounded source prefix and clips whole
// graphemes to terminal cells. A source-cut final cluster is omitted: its unseen
// suffix could contain combining marks or more of a ZWJ sequence.
func pendingInputPreview(label, source string, width int) string {
	if width <= 0 {
		return ""
	}
	label = ansi.Truncate(label, width, "")
	width -= ansi.StringWidth(label)
	if width <= 0 {
		return label
	}
	cut := len(source) > pendingPreviewBytes
	if cut {
		source = source[:pendingPreviewBytes]
		// Do not manufacture a replacement character by cutting a UTF-8 rune.
		// Invalid trailing bytes are also safely omitted from this preview.
		for len(source) > 0 {
			r, size := utf8.DecodeLastRuneInString(source)
			if r != utf8.RuneError || size != 1 {
				break
			}
			source = source[:len(source)-1]
		}
	}
	text := strings.Join(strings.Fields(render.Clean(source)), " ")
	end := 0
	for rest := text; rest != ""; {
		cluster, cells := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
		if cells > width || cut && len(cluster) == len(rest) {
			break
		}
		end += len(cluster)
		width -= cells
		rest = rest[len(cluster):]
	}
	return label + text[:end]
}
