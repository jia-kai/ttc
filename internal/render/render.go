// Package render provides portable, control-safe Markdown presentation.
package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Clean strips terminal controls while preserving line breaks and tabs in blocks.
func Clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || r == 0x7f || r == 0x202e || r == 0x202d || r == 0x202a || r == 0x202b || r == 0x202c || r == 0x2066 || r == 0x2067 || r == 0x2068 || r == 0x2069 {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
}

// EscapeInline escapes untrusted text without truncating document content.
func EscapeInline(s string) string {
	s = Clean(s)
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\t", " ")
	s = strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;", "|", "\\|").Replace(s)
	return s
}

// Inline escapes and bounds concise card labels; document bodies use EscapeInline.
func Inline(s string) string {
	s = EscapeInline(s)
	if len(s) > 150 {
		s = s[:150]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		s += "…"
	}
	return s
}

// Fence chooses a delimiter longer than any backtick run in the body.
func Fence(s, lang string) string {
	if lang == "json" {
		s = prettyJSON(json.RawMessage(s))
	}
	f := backticks(s, 3)
	return f + lang + "\n" + Clean(s) + "\n" + f + "\n"
}

func backticks(s string, minimum int) string {
	n, max := 0, minimum-1
	for _, r := range s {
		if r == '`' {
			n++
			if n > max {
				max = n
			}
		} else {
			n = 0
		}
	}
	return strings.Repeat("`", max+1)
}

func inlineCode(s string) string {
	s = clip(strings.Join(strings.Fields(Clean(s)), " "), 128)
	f := backticks(s, 1)
	return f + " " + s + " " + f
}

// Markdown is a durable presentation record, distinct from the model result.
// Summary is bounded portable Markdown for a card; Detail is the expanded inspector.
type Markdown struct {
	Revision int     `json:"revision"`
	Summary  string  `json:"summary"`
	Detail   string  `json:"detail"`
	Export   *string `json:"export,omitempty"` // Optional export body; nil delegates to the inspector, empty omits it.
}

// DiffBriefing identifies generated tool summaries with a short fenced diff.
// Other summaries retain a single-row layout even when they contain code fences.
func DiffBriefing(summary string) bool {
	_, body, found := strings.Cut(summary, "\n\n")
	return found && strings.HasPrefix(strings.TrimLeft(body, "`"), "diff\n")
}

// ExportText returns a custom export body or the current inspector presentation.
func (m Markdown) ExportText() string {
	if m.Export != nil {
		return *m.Export
	}
	return m.Detail
}

// Tool renders a compact briefing and readable supplied parameters/results.
// Shell commands retain language metadata for highlighting; captured output is
// escaped and labeled. TerminalBriefing displays summaries as one clipped row.
func Tool(name string, args, result json.RawMessage) Markdown {
	var a, v map[string]any
	_ = json.Unmarshal(args, &a)
	_ = json.Unmarshal(result, &v)
	summary := "**" + Inline(name) + "**"
	detail := toolDetail(name, args, result)
	if name != "glob" || v["status"] == "running" {
		if status, ok := v["status"].(string); ok {
			if status == "running" && a["background"] == true && v["job_id"] != nil {
				status = "started"
			}
			summary += " · " + Inline(Status(status))
		}
		if code, ok := v["exit_code"].(float64); ok {
			summary += fmt.Sprintf(" · exit %d", int(code))
		}
	}
	if failure, ok := v["error"].(map[string]any); ok {
		summary += " · error"
		if message, ok := failure["message"].(string); ok {
			summary += ": " + Inline(message)
		}
	}
	if name == "shell" {
		if command, ok := a["command"].(string); ok {
			summary += "\n\n" + Fence(clip(strings.Join(strings.Fields(Clean(command)), " "), 192), "sh")
		}
	} else {
		// Prefer the pattern over an optional search root; never preview file
		// contents, question arrays, or full results in the conversation row.
		for _, key := range []string{"query", "pattern", "path", "label", "name", "job_id", "url", "document_id"} {
			if value, ok := a[key].(string); ok && value != "" {
				summary += " · " + inlineCode(value)
				break
			}
		}
	}
	if name == "shell" || name == "subagent" {
		for _, stream := range []string{"stdout", "stderr"} {
			if text, ok := v[stream].(string); ok && text != "" {
				text = strings.Join(strings.Fields(Clean(text)), " ")
				if len(text) > 64 {
					text = text[len(text)-64:]
					for !utf8.ValidString(text) {
						text = text[1:]
					}
					text = "…" + text
				}
				summary += " · **" + stream + "**: " + Inline(text)
			}
		}
		if v["truncated"] == true {
			summary += " · tail truncated"
		}
		if id, ok := v["job_id"].(string); ok {
			summary += " · " + inlineCode(id)
		}
	}
	return Markdown{Revision: 1, Summary: summary, Detail: detail}
}

func clip(s string, limit int) string {
	s = Clean(s)
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

func prettyJSON(raw json.RawMessage) string {
	var formatted bytes.Buffer
	if json.Indent(&formatted, raw, "", "  ") != nil {
		return string(raw)
	}
	return formatted.String()
}
