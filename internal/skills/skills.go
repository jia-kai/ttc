// Package skills discovers local and embedded instructions with explicit precedence.
package skills

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"ttc/internal/prompts"
	bundledskills "ttc/prompt/skills"
)

// Skill identifies a discovered document; Content is loaded only on request.
type Skill struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Source      string `json:"source"`
	Content     string `json:"content,omitempty"`
	Description string `json:"description,omitempty"`
	// Roles is the discovery-time audience; nil means all runtime roles.
	Roles []Role `json:"roles,omitempty"`
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
	entries, e := fs.Glob(bundledskills.Files, "*/SKILL.md")
	if e != nil {
		return nil, e
	}
	for _, p := range entries {
		b, e := bundledskills.Files.ReadFile(p)
		if e != nil {
			return nil, e
		}
		name := strings.Split(p, "/")[0]
		description, roles, err := metadata(string(b))
		if err != nil {
			return nil, fmt.Errorf(prompts.SkillMetadataFailure, name, err)
		}
		c.items[name] = Skill{Name: name, Path: "prompt/skills/" + p, Source: "bundled", Description: description, Roles: roles}
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
			description, roles, err := metadata(string(b))
			if err != nil {
				return nil, fmt.Errorf(prompts.SkillMetadataFailure, name, err)
			}
			c.items[name] = Skill{Name: name, Path: p, Source: layer.source, Description: description, Roles: roles}
		}
	}
	return c, nil
}

// List returns sorted, defensively copied discovery metadata for actor, without
// instruction bodies. Selection happens before role filtering; there is no fallback.
func (c *Catalog) List(actor string) ([]Skill, error) {
	role, err := RoleForActor(actor)
	if err != nil {
		return nil, err
	}
	out := make([]Skill, 0, len(c.items))
	for _, s := range c.items {
		if !allows(s.Roles, role) {
			continue
		}
		s.Roles = append([]Role(nil), s.Roles...)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Load reads the selected source at tool execution time. Local sources use
// ReadInstruction's regular-file, size and cancellation constraints. Both the
// discovery snapshot and current file must permit actor's role: file edits can
// revoke access, but relaxing discovery restrictions requires rediscovery.
// Failures return no metadata or content; source paths are not included in errors.
func (c *Catalog) Load(ctx context.Context, name, actor string) (Skill, error) {
	if err := ctx.Err(); err != nil {
		return Skill{}, err
	}
	role, err := RoleForActor(actor)
	if err != nil {
		return Skill{}, err
	}
	s, ok := c.items[name]
	if !ok {
		return Skill{}, fmt.Errorf(prompts.SkillNotFound, name)
	}
	if !allows(s.Roles, role) {
		return Skill{}, fmt.Errorf(prompts.SkillRoleDenied, name, role)
	}
	var b []byte
	var e error
	if s.Source == "bundled" {
		b, e = bundledskills.Files.ReadFile(strings.TrimPrefix(s.Path, "prompt/skills/"))
	} else {
		b, e = ReadInstruction(ctx, s.Path)
	}
	if e != nil {
		if err := ctx.Err(); err != nil {
			return Skill{}, err
		}
		return Skill{}, fmt.Errorf(prompts.SkillLoadFailure, name)
	}
	_, roles, err := metadata(string(b))
	if err != nil {
		return Skill{}, fmt.Errorf(prompts.SkillMetadataFailure, name, err)
	}
	if !allows(roles, role) {
		return Skill{}, fmt.Errorf(prompts.SkillRoleDenied, name, role)
	}
	s.Roles = append([]Role(nil), s.Roles...)
	s.Content = string(b)
	return s, nil
}
