package tool

import (
	"context"
	"scicode/internal/skills"
)

// AddSkills exposes exact-name instruction loading from the selected catalog.
func AddSkills(r *Registry, c *skills.Catalog) {
	type args struct {
		Name string `json:"name"`
	}
	Register(r, "skill", "Load a named SKILL.md before following its instructions. Use a listed exact name.", map[string]any{"name": Property("string")}, []string{"name"}, func(a args) error { return Required("name", a.Name) }, func(ctx context.Context, x Execution, a args) (any, error) {
		s, e := c.Load(ctx, a.Name)
		if e != nil {
			return nil, Fail("skill_unavailable", e.Error())
		}
		return map[string]any{"name": s.Name, "path": s.Path, "source": s.Source, "content": s.Content}, nil
	})
}
