package render

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"ttc/internal/llm"
)

// InspectionVersion identifies the portable readable projection. Persisted
// artifact/cache keys must include it; provider captures remain exact.
const InspectionVersion = 3

const inspectionWidth = 80

// Inspection renders provider-neutral sections as compact plain UTF-8. Long
// text/containers use counted summaries; expanded and raw views retain details.
// Short scalar values stay in flow lists, targeting 80 cells and wrapping in the
// viewer rather than introducing indexed entries for individual words.
func Inspection(doc llm.InspectionDocument) string { return inspectionDocument(doc, false) }

// InspectionExpanded renders every ordinary text line and container entry.
// Opaque binary/encrypted data remains a byte-count/hash summary; exact bytes
// are accessible separately through raw capture. Terminal controls are cleaned.
func InspectionExpanded(doc llm.InspectionDocument) string { return inspectionDocument(doc, true) }

func inspectionDocument(doc llm.InspectionDocument, expanded bool) string {
	var b strings.Builder
	for i, section := range doc.Sections {
		if i != 0 {
			b.WriteByte('\n')
		}
		if section.Title != "" {
			b.WriteString(Clean(section.Title) + "\n")
		}
		inspectionFields(&b, section.Fields, 1, expanded)
	}
	return strings.TrimRight(b.String(), "\n")
}

func inspectionFields(b *strings.Builder, fields []llm.InspectionField, depth int, expanded bool) {
	pad := strings.Repeat("  ", depth)
	head, tail := inspectionRange(len(fields), 24, 8, 4, expanded)
	for i, field := range fields {
		if i >= head && i < len(fields)-tail {
			if i == head {
				fmt.Fprintf(b, "%s[… %d of %d fields omitted]\n", pad, len(fields)-head-tail, len(fields))
			}
			continue
		}
		name := Clean(field.Name)
		if !inspectionIdentifier(name) {
			name = strconv.Quote(name)
		}
		prefix := pad + name + ":"
		inspectionLabeled(b, prefix, field.Value, depth+1, expanded)
	}
}

func inspectionLabeled(b *strings.Builder, prefix string, v llm.InspectionValue, depth int, expanded bool) {
	if scalar, ok := inspectionInline(v, inspectionWidth-ansi.StringWidth(prefix)-1, expanded); ok {
		b.WriteString(prefix + " " + scalar + "\n")
	} else {
		if v.Kind == "string" {
			prefix += " |"
		}
		b.WriteString(prefix + "\n")
		inspectionValue(b, v, depth, expanded)
	}
}

func inspectionRange(n, threshold, head, tail int, expanded bool) (int, int) {
	if expanded || n <= threshold {
		return n, 0
	}
	return head, tail
}

func inspectionValue(b *strings.Builder, v llm.InspectionValue, depth int, expanded bool) {
	pad := strings.Repeat("  ", depth)
	if !expanded && depth > 32 {
		fmt.Fprintf(b, "%s[nested content omitted]\n", pad)
		return
	}
	if scalar, ok := inspectionInline(v, inspectionWidth-len(pad), expanded); ok {
		b.WriteString(pad + scalar + "\n")
		return
	}
	switch v.Kind {
	case "object":
		inspectionFields(b, v.Fields, depth, expanded)
	case "array":
		head, tail := inspectionRange(len(v.Items), 16, 4, 4, expanded)
		for i, value := range v.Items {
			if i >= head && i < len(v.Items)-tail {
				if i == head {
					fmt.Fprintf(b, "%s[… %d of %d items omitted]\n", pad, len(v.Items)-head-tail, len(v.Items))
				}
				continue
			}
			inspectionLabeled(b, fmt.Sprintf("%s[%d]", pad, i+1), value, depth+1, expanded)
		}
	default:
		lines := strings.Split(Clean(v.Text), "\n")
		head, tail := inspectionRange(len(lines), 10, 4, 2, expanded)
		for i, line := range lines {
			if i >= head && i < len(lines)-tail {
				if i == head {
					fmt.Fprintf(b, "%s[… %d of %d lines omitted]\n", pad, len(lines)-head-tail, len(lines))
				}
				continue
			}
			if !expanded {
				line = inspectionShortText(line)
			}
			b.WriteString(pad + line + "\n")
		}
	}
}

func inspectionInline(v llm.InspectionValue, width int, expanded bool) (string, bool) {
	var s string
	switch v.Kind {
	case "opaque":
		return fmt.Sprintf("[opaque payload: %d bytes; sha256 %x; see raw capture]", len(v.Text), sha256.Sum256([]byte(v.Text))), true
	case "null":
		s = "null"
	case "number", "bool":
		s = v.Text
	case "string":
		if strings.ContainsAny(v.Text, "\r\n") {
			return "", false
		}
		text := Clean(v.Text)
		if !expanded {
			text = inspectionShortText(text)
		}
		s = strconv.Quote(text)
	case "object":
		if len(v.Fields) != 0 {
			return "", false
		}
		s = "{}"
	case "array":
		fold := !expanded && len(v.Items) > 16
		parts := make([]string, 0, min(len(v.Items), 16))
		for i, item := range v.Items {
			if item.Kind == "array" || item.Kind == "object" || item.Kind == "opaque" {
				return "", false
			}
			part, ok := inspectionInline(item, width, expanded)
			if !ok || ansi.StringWidth(part) > 48 {
				return "", false
			}
			if fold && i >= 3 && i < len(v.Items)-2 {
				if i == 3 {
					parts = append(parts, fmt.Sprintf("… %d omitted", len(v.Items)-5))
				}
				continue
			}
			parts = append(parts, part)
		}
		s = "[" + strings.Join(parts, ", ") + "]"
		if !expanded && !fold && len(parts) > 6 && ansi.StringWidth(s) > width {
			tail := parts[len(parts)-2:]
			short := append(parts[:3:3], fmt.Sprintf("… %d omitted", len(parts)-5))
			parts = append(short, tail...)
			s = "[" + strings.Join(parts, ", ") + "]"
		}
	default:
		return "", false
	}
	return s, true
}

func inspectionShortText(text string) string {
	if ansi.StringWidth(text) <= 240 {
		return text
	}
	runes := []rune(text)
	head, cells := 0, 0
	for head < len(runes) {
		width := runewidth.RuneWidth(runes[head])
		if cells+width > 160 {
			break
		}
		head++
		cells += width
	}
	tail, cells := len(runes), 0
	for tail > head {
		width := runewidth.RuneWidth(runes[tail-1])
		if cells+width > 40 {
			break
		}
		tail--
		cells += width
	}
	return string(runes[:head]) + fmt.Sprintf("[… %d characters omitted]", tail-head) + string(runes[tail:])
}

func inspectionIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if r != '_' && !unicode.IsLetter(r) && (i == 0 || !unicode.IsDigit(r)) {
			return false
		}
	}
	return true
}
