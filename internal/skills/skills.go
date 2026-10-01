// Package skills discovers local and embedded instructions with explicit precedence.
package skills

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	defaultskills "scicode/default-skills"
	"sort"
	"strings"
)

// Skill identifies a discovered document; Content is loaded only on request.
type Skill struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Source      string `json:"source"`
	Content     string `json:"content,omitempty"`
	Description string `json:"description,omitempty"`
}

// Catalog holds project > user > bundled selections by exact name.
type Catalog struct{ items map[string]Skill }

// Discover loads metadata from bundled skills, the user directory, and each
// ancestor's .agents/skill and .agents/skills directories through project cwd.
// Nearer project directories override parents and user skills by exact name;
// plural skills directories take precedence over singular ones at the same level.
// Local documents must be regular files of at most 1 MiB; symlinks are followed.
func Discover(ctx context.Context, project, user string) (*Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c := &Catalog{items: map[string]Skill{}}
	entries, e := fs.Glob(defaultskills.Files, "*/SKILL.md")
	if e != nil {
		return nil, e
	}
	for _, p := range entries {
		b, e := defaultskills.Files.ReadFile(p)
		if e != nil {
			return nil, e
		}
		name := strings.Split(p, "/")[0]
		c.items[name] = Skill{Name: name, Path: "default-skills/" + p, Source: "bundled", Description: description(string(b))}
	}
	type layer struct{ root, source string }
	var layers []layer
	if user != "" {
		if filepath.Base(user) == "skills" {
			layers = append(layers, layer{filepath.Join(filepath.Dir(user), "skill"), "user"})
		}
		layers = append(layers, layer{user, "user"})
	}
	project, e = filepath.Abs(project)
	if e != nil {
		return nil, fmt.Errorf("resolve skill directory: %w", e)
	}
	var ancestors []string
	for dir := project; ; dir = filepath.Dir(dir) {
		ancestors = append(ancestors, dir)
		if dir == filepath.Dir(dir) {
			break
		}
	}
	for i := len(ancestors) - 1; i >= 0; i-- {
		for _, name := range []string{"skill", "skills"} {
			layers = append(layers, layer{filepath.Join(ancestors[i], ".agents", name), "project"})
		}
	}
	for _, layer := range layers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if layer.root == "" {
			continue
		}
		matches, e := filepath.Glob(filepath.Join(layer.root, "*", "SKILL.md"))
		if e != nil {
			return nil, e
		}
		for _, p := range matches {
			b, e := ReadInstruction(ctx, p)
			if e != nil {
				return nil, e
			}
			name := filepath.Base(filepath.Dir(p))
			c.items[name] = Skill{Name: name, Path: p, Source: layer.source, Description: description(string(b))}
		}
	}
	return c, nil
}
func description(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "description:") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "description:")), "\"'")
		}
	}
	return ""
}

// List returns sorted metadata without full instruction bodies.
func (c *Catalog) List() []Skill {
	out := make([]Skill, 0, len(c.items))
	for _, s := range c.items {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Load reads the selected source at tool execution time. Local sources use
// ReadInstruction's regular-file, size and cancellation constraints.
func (c *Catalog) Load(ctx context.Context, name string) (Skill, error) {
	if err := ctx.Err(); err != nil {
		return Skill{}, err
	}
	s, ok := c.items[name]
	if !ok {
		return s, fmt.Errorf("skill %q not found", name)
	}
	var b []byte
	var e error
	if s.Source == "bundled" {
		b, e = defaultskills.Files.ReadFile(strings.TrimPrefix(s.Path, "default-skills/"))
	} else {
		b, e = ReadInstruction(ctx, s.Path)
	}
	s.Content = string(b)
	return s, e
}
