package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"ttc/internal/prompts"
	"unicode"
)

// toolDetail formats supplied parameters and results without exposing JSON
// serialization syntax as the primary presentation. Exact JSON lives in records.
func toolDetail(name string, args, result json.RawMessage) string {
	return fmt.Sprintf(prompts.ToolDetailSections, toolValues(args, name), toolValues(result, ""))
}

func toolValues(raw json.RawMessage, name string) string {
	if len(raw) == 0 {
		return prompts.ToolDetailPending
	}
	var value any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		return Fence(string(raw), "text")
	}
	var out strings.Builder
	path := ""
	if object, ok := value.(map[string]any); ok {
		path, _ = object["path"].(string)
		if id, _ := object["job_id"].(string); id != "" && name == "" {
			// The runtime expands named captures once, within its inspector cap.
			// Omit the model's shorter duplicate excerpts from the base detail.
			delete(object, "stdout")
			delete(object, "stderr")
		}
		if name == "shell" {
			if _, supplied := object["strict"]; !supplied {
				object["strict"] = true
			}
		}
	}
	writeToolValue(&out, "", value, "", path)
	if out.Len() == 0 {
		return prompts.ToolDetailNone
	}
	return out.String()
}

func writeToolValue(out *strings.Builder, key string, value any, indent, path string) {
	label := strings.ReplaceAll(key, "_", " ")
	if label != "" {
		runes := []rune(label)
		runes[0] = unicode.ToUpper(runes[0])
		label = string(runes)
		out.WriteString(indent + "- **" + EscapeInline(label) + ":**")
	}
	switch value := value.(type) {
	case map[string]any:
		if key != "" {
			out.WriteString("\n")
			indent += "  "
		}
		if len(value) == 0 && key != "" {
			out.WriteString(indent + prompts.ToolDetailEmpty)
		}
		keys := make([]string, 0, len(value))
		for k := range value {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			writeToolValue(out, k, value[k], indent, path)
		}
	case []any:
		out.WriteString("\n")
		if len(value) == 0 {
			out.WriteString(indent + prompts.ToolDetailEmptyList)
		}
		for i, item := range value {
			writeToolValue(out, fmt.Sprint(i+1), item, indent+"  ", path)
		}
	case string:
		if key == "status" {
			value = Status(value)
		}
		lang := ""
		switch key {
		case "command":
			lang = "sh"
		case "patch_text":
			lang = "diff"
		case "content", "old_text", "new_text":
			lang = strings.TrimPrefix(filepath.Ext(path), ".")
		}
		if lang != "" || strings.ContainsAny(value, "\n\t") || len(value) > 160 {
			if lang == "" {
				lang = "text"
			}
			out.WriteString("\n\n")
			for _, row := range strings.Split(strings.TrimSuffix(Fence(value, lang), "\n"), "\n") {
				out.WriteString(indent + "  " + row + "\n")
			}
			out.WriteString("\n")
		} else {
			f := backticks(value, 1)
			out.WriteString(" " + f + " " + Clean(value) + " " + f + "\n")
		}
	default:
		out.WriteString(" " + EscapeInline(fmt.Sprint(value)) + "\n")
	}
}
