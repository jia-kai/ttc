package render

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// ResponsePrefix returns a bounded, control-safe single-line response preview.
// It reads only the initial 256 code points, even for very large responses.
func ResponsePrefix(text string) string {
	var out strings.Builder
	space := false
	n := 0
	for _, r := range text {
		if n >= 256 {
			break
		}
		n++
		if unicode.IsSpace(r) {
			space = out.Len() > 0
			continue
		}
		if Clean(string(r)) == "" {
			continue
		}
		if space {
			out.WriteByte(' ')
			space = false
		}
		out.WriteRune(r)
	}
	return out.String()
}

// SubagentResponse presents one response row bounded by terminal cell width.
// Full message contents belong to the caller's inspection source, not this row.
func SubagentResponse(actor, name, speaker, text string, width int, colors bool) string {
	badge := SubagentBadge(actor, name, max(1, width/2), colors)
	row := badge + " " + speaker + " " + ResponsePrefix(text) + " ..."
	return ansi.Truncate(row, max(1, width), "…")
}

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
