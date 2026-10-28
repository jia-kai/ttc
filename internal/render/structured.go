package render

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// StructuredState carries bounded source syntax hints across Unicode-character
// pages. The zero value starts a document. Partial holds at most 4 KiB of a
// split source row; ordinary large text is never retained between pages.
type StructuredState struct {
	MidRow      bool
	TextBlock   bool
	BlockIndent int // Source spaces before the key/index introducing a | block.
	Partial     string
}

// Structured styles one source fragment. A continuation's first partial row is
// literal; callers paging a whole document use StructuredPage to carry | blocks.
// Generated SGR belongs only in a view cache, never in saved readable artifacts.
func Structured(input string, continuation, colors bool) string {
	text, _ := StructuredPage(input, StructuredState{MidRow: continuation}, colors)
	return text
}

// StructuredPage styles control-safe inspection source without changing its
// text or interpreting Markdown. Returned state belongs to the next source page;
// colors=false still advances syntax hints but emits no terminal controls.
func StructuredPage(input string, state StructuredState, colors bool) (string, StructuredState) {
	input = Clean(input)
	rows := strings.Split(input, "\n")
	for i, row := range rows {
		complete := i < len(rows)-1
		if i == 0 && state.MidRow {
			combined := state.Partial + row
			if state.Partial == "" {
				combined = ""
			}
			if complete {
				if combined != "" {
					_, state = structuredRow(combined, state, false)
				}
				state.MidRow, state.Partial = false, ""
			} else {
				state.Partial = structuredPartial(combined)
			}
			continue
		}
		styled, next := structuredRow(row, state, colors)
		rows[i] = styled
		if complete {
			state = next
			state.MidRow, state.Partial = false, ""
		} else if row != "" {
			state.MidRow, state.Partial = true, structuredPartial(row)
		}
	}
	return strings.Join(rows, "\n"), state
}

func structuredPartial(row string) string {
	if len(row) > 4096 {
		return ""
	}
	return strings.Clone(row)
}

func structuredRow(row string, state StructuredState, colors bool) (string, StructuredState) {
	body := strings.TrimLeft(row, " ")
	pad := row[:len(row)-len(body)]
	if body == "" {
		return row, state
	}
	if state.TextBlock {
		if len(pad) > state.BlockIndent {
			return row, state
		}
		state.TextBlock = false
	}
	color := func(text, shade string) string {
		if !colors {
			return text
		}
		return inspectionColor(text, shade)
	}
	if strings.HasPrefix(body, "[opaque payload:") || strings.HasPrefix(body, "[… ") || strings.HasPrefix(body, "[nested content omitted;") {
		return pad + color(body, MutedColor), state
	}
	if pad == "" {
		if colors && structuredFragment(body) {
			return structuredJSON(row), state
		}
		return color(row, CyanColor), state
	}
	end := -1
	if body[0] == '"' {
		for j := 1; j < len(body); j++ {
			if body[j] == '\\' {
				j++
				continue
			}
			if body[j] == '"' {
				if j+1 < len(body) && body[j+1] == ':' && json.Valid([]byte(body[:j+1])) {
					end = j + 1
				}
				break
			}
		}
	} else if colon := strings.IndexByte(body, ':'); colon >= 0 && inspectionIdentifier(body[:colon]) {
		end = colon
	}
	value := ""
	prefix := ""
	if end >= 0 && (end+1 == len(body) || body[end+1] == ' ') {
		prefix = pad + color(body[:end], BlueColor) + color(":", MutedColor)
		value = body[end+1:]
	} else if body[0] == '[' {
		if close := strings.IndexByte(body, ']'); close > 1 {
			if _, err := strconv.Atoi(body[1:close]); err == nil {
				prefix = pad + color(body[:close+1], MutedColor)
				value = body[close+1:]
			}
		}
	}
	if prefix == "" {
		if colors && structuredFragment(body) {
			return pad + structuredJSON(body), state
		}
		return row, state
	}
	if strings.TrimSpace(value) == "|" {
		state.TextBlock, state.BlockIndent = true, len(pad)
		return prefix + color(value, MutedColor), state
	}
	if colors {
		value = structuredJSON(value)
	}
	return prefix + value, state
}

// Indented native JSON contains standalone array values and incomplete object
// delimiters. These are syntax fragments, not complete JSON documents.
func structuredFragment(source string) bool {
	source = strings.TrimSpace(source)
	return source != "" && (strings.ContainsRune("{[}]", rune(source[0])) || json.Valid([]byte(strings.TrimSuffix(source, ","))))
}

func structuredJSON(source string) string {
	if strings.HasPrefix(strings.TrimSpace(source), "[opaque payload:") {
		return inspectionColor(source, MutedColor)
	}
	// Pages may stop inside any token. A general-purpose lexer recovers from
	// an unfinished string one character at a time, allocating lexer state for
	// each character. Scan JSON tokens directly so partial strings take one
	// pass and one styled span, just like complete strings.
	var b strings.Builder
	b.Grow(len(source))
	for start := 0; start < len(source); {
		end := start + 1
		color := ""
		switch source[start] {
		case '"':
			for end < len(source) {
				c := source[end]
				end++
				if c == '\\' && end < len(source) {
					end++
				} else if c == '"' {
					break
				}
			}
			color = AmberColor
		case '{', '}', '[', ']', ':', ',':
			color = MutedColor
		case ' ', '\t', '\r', '\n':
			for end < len(source) && strings.ContainsRune(" \t\r\n", rune(source[end])) {
				end++
			}
		default:
			// Whitespace and non-JSON prose remain literal. Consume whole
			// scalar candidates so embedded words/numbers are not colored.
			for end < len(source) && !strings.ContainsRune(" \t\r\n\"{}[]:,", rune(source[end])) {
				end++
			}
			token := source[start:end]
			if token == "true" || token == "false" || token == "null" {
				color = LavenderColor
			} else if (source[start] == '-' || source[start] >= '0' && source[start] <= '9') && json.Valid([]byte(token)) {
				color = CyanColor
			}
		}
		if color == "" {
			b.WriteString(source[start:end])
		} else {
			b.WriteString(inspectionColor(source[start:end], color))
		}
		start = end
	}
	return b.String()
}

func inspectionColor(s, color string) string {
	rgb, err := strconv.ParseUint(strings.TrimPrefix(color, "#"), 16, 24)
	if err != nil {
		return s
	}
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[0m", rgb>>16, rgb>>8&255, rgb&255, s)
}
