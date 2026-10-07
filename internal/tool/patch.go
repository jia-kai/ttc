package tool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"ttc/internal/prompts"
	"ttc/internal/workspace"
)

type patchArgs struct {
	Text string `json:"patch_text"`
}
type patchSection struct {
	path, move, action string
	lines              []string
}

func parsePatch(text string) ([]patchSection, error) {
	if err := validText(text); err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) < 3 || lines[0] != "*** Begin Patch" || lines[len(lines)-1] != "*** End Patch" {
		return nil, errors.New(prompts.ToolPatchMarkers)
	}
	var sections []patchSection
	var cur *patchSection
	seen := map[string]bool{}
	for _, line := range lines[1 : len(lines)-1] {
		action, path := "", ""
		for prefix, a := range map[string]string{"*** Add File: ": "add", "*** Update File: ": "update", "*** Delete File: ": "delete"} {
			if strings.HasPrefix(line, prefix) {
				action, path = a, strings.TrimPrefix(line, prefix)
				break
			}
		}
		if action != "" {
			if path == "" || seen[path] {
				return nil, errors.New(prompts.ToolPatchDuplicateTarget)
			}
			seen[path] = true
			sections = append(sections, patchSection{path: path, action: action})
			cur = &sections[len(sections)-1]
			continue
		}
		if cur == nil {
			return nil, errors.New(prompts.ToolPatchHeaderRequired)
		}
		if strings.HasPrefix(line, "*** Move to: ") {
			if cur.action != "update" || cur.move != "" || len(cur.lines) > 0 {
				return nil, errors.New(prompts.ToolPatchMoveHeader)
			}
			cur.move = strings.TrimPrefix(line, "*** Move to: ")
			if cur.move == "" || seen[cur.move] {
				return nil, errors.New(prompts.ToolPatchMoveTarget)
			}
			seen[cur.move] = true
			continue
		}
		cur.lines = append(cur.lines, line)
	}
	if len(sections) == 0 {
		return nil, errors.New(prompts.ToolPatchEmpty)
	}
	return sections, nil
}
func updatePatch(data []byte, lines []string) ([]byte, error) {
	text := string(data)
	if e := validText(text); e != nil {
		return nil, e
	}
	if len(lines) == 0 {
		return data, nil
	}
	cursor := 0
	for i := 0; i < len(lines); {
		if !strings.HasPrefix(lines[i], "@@") {
			return nil, errors.New(prompts.ToolPatchHunkRequired)
		}
		anchor := strings.TrimSpace(strings.TrimPrefix(lines[i], "@@"))
		i++
		if anchor != "" {
			pos := strings.Index(text[cursor:], anchor)
			if pos < 0 {
				return nil, errors.New(prompts.ToolPatchAnchorMissing)
			}
			cursor += pos + len(anchor)
			if cursor < len(text) && text[cursor] == '\n' {
				cursor++
			}
		}
		var old, new strings.Builder
		endFile := false
		for i < len(lines) && !strings.HasPrefix(lines[i], "@@") {
			line := lines[i]
			i++
			if line == "*** End of File" {
				endFile = true
				continue
			}
			if len(line) == 0 {
				return nil, errors.New(prompts.ToolPatchEmptyHunkLine)
			}
			switch line[0] {
			case ' ':
				old.WriteString(line[1:] + "\n")
				new.WriteString(line[1:] + "\n")
			case '-':
				old.WriteString(line[1:] + "\n")
			case '+':
				new.WriteString(line[1:] + "\n")
			default:
				return nil, errors.New(prompts.ToolPatchHunkPrefix)
			}
		}
		before, after := old.String(), new.String()
		if before == "" {
			return nil, errors.New(prompts.ToolPatchContextRequired)
		}
		pos := patchLineMatch(text, before, cursor, endFile)
		if pos < 0 && strings.HasSuffix(before, "\n") {
			before = strings.TrimSuffix(before, "\n")
			after = strings.TrimSuffix(after, "\n")
			pos = patchLineMatch(text, before, cursor, true)
		}
		if pos < 0 {
			return nil, errors.New(prompts.ToolPatchContextMismatch)
		}
		text = text[:pos] + after + text[pos+len(before):]
		cursor = pos + len(after)
	}
	return []byte(text), nil
}

// patchLineMatch finds exact physical-line context after cursor. Without a
// trailing newline, context may match only the final unterminated file line.
func patchLineMatch(text, needle string, cursor int, eof bool) int {
	if needle == "" {
		return -1
	}
	if eof {
		pos := len(text) - len(needle)
		if pos >= cursor && strings.HasSuffix(text, needle) && (pos == 0 || text[pos-1] == '\n') {
			return pos
		}
		return -1
	}
	for cursor <= len(text) {
		rel := strings.Index(text[cursor:], needle)
		if rel < 0 {
			return -1
		}
		pos := cursor + rel
		if pos == 0 || text[pos-1] == '\n' {
			return pos
		}
		cursor = pos + 1
	}
	return -1
}
func addPatch(r *Registry, w *workspace.Manager) {
	Register(r, "patch", prompts.ToolDescription("patch"), map[string]any{"patch_text": Property("string")}, []string{"patch_text"}, func(a patchArgs) error { _, e := parsePatch(a.Text); return e }, func(ctx context.Context, x Execution, a patchArgs) (any, error) {
		sections, _ := parsePatch(a.Text)
		var ops []workspace.Mutation
		files := []map[string]string{}
		for _, s := range sections {
			switch s.action {
			case "add":
				var b strings.Builder
				for _, line := range s.lines {
					if !strings.HasPrefix(line, "+") {
						return nil, errors.New(prompts.ToolPatchAddPrefix)
					}
					b.WriteString(line[1:] + "\n")
				}
				ops = append(ops, workspace.Mutation{Path: s.path, Data: []byte(b.String()), MustAbsent: true})
			case "delete":
				if len(s.lines) != 0 {
					return nil, errors.New(prompts.ToolPatchDeleteContent)
				}
				ops = append(ops, workspace.Mutation{Path: s.path, Delete: true, MustExist: true})
			case "update":
				section := s
				if s.move != "" {
					ops = append(ops, workspace.Mutation{Path: s.path, Delete: true, MustExist: true}, workspace.Mutation{Path: s.move, Source: s.path, MustAbsent: true, Transform: func(b []byte) ([]byte, error) { return updatePatch(b, section.lines) }})
					files = append(files, map[string]string{"path": w.Path(s.move), "action": "move"})
					continue
				}
				ops = append(ops, workspace.Mutation{Path: s.path, MustExist: true, Transform: func(b []byte) ([]byte, error) { return updatePatch(b, section.lines) }})
			}
			files = append(files, map[string]string{"path": w.Path(s.path), "action": s.action})
		}
		res, e := w.Apply(ctx, x.SessionID, x.CallID, ops)
		if e != nil && res.ChangeID != 0 {
			applied := []string{}
			for _, p := range res.Changes {
				if p.Applied {
					applied = append(applied, p.Path)
				}
			}
			return presentFiles(ctx, nil, res.Changes), &Error{Code: "partial_patch", Message: fmt.Sprintf(prompts.ToolPartialPatch, e), Details: map[string]any{"applied": applied}}
		}
		return presentFiles(ctx, map[string]any{"files": files}, res.Changes), e
	})
}
