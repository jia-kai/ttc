package render

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// SubagentBadge formats a model-chosen name for a terminal, bounded by width in
// cells. Names longer than 26 Unicode code points become 23 plus "...". Colors
// are stable per actor identity; colors=false produces control-free plain text.
func SubagentBadge(actor, name string, width int, colors bool) string {
	name = strings.Join(strings.Fields(Clean(name)), " ")
	runes := []rune(name)
	if len(runes) > 26 {
		name = string(runes[:23]) + "..."
	}
	text := ansi.Truncate("[Sub "+name+"]", max(1, width), "…")
	if !colors {
		return text
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(actor))
	palette := [...]string{BlueColor, CyanColor, LavenderColor, AmberColor}
	rgb, err := strconv.ParseUint(palette[h.Sum32()%uint32(len(palette))][1:], 16, 24)
	if err != nil {
		panic("invalid subagent palette: " + err.Error())
	}
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[39m", rgb>>16, (rgb>>8)&255, rgb&255, text)
}

// Status returns a human-readable tool/job state without changing canonical
// protocol values. Only successful completion has a distinct display spelling.
func Status(status string) string {
	if status == "completed" {
		return "done"
	}
	return status
}
