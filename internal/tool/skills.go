package tool

import (
	"context"
	"fmt"
	"ttc/internal/prompts"
	"ttc/internal/skills"
)

// AddSkills exposes exact-name instruction loading from the selected catalog.
func AddSkills(r *Registry, c *skills.Catalog) {
	type args struct {
		Name string `json:"name"`
	}
	Register(r, "skill", prompts.ToolDescription("skill"), map[string]any{"name": Property("string")}, []string{"name"}, func(a args) error { return Required("name", a.Name) }, func(ctx context.Context, x Execution, a args) (any, error) {
		s, e := c.Load(ctx, a.Name)
		if e != nil {
			return nil, Fail("skill_unavailable", fmt.Sprintf(prompts.ToolSkillUnavailable, e.Error()))
		}
		return map[string]any{"name": s.Name, "path": s.Path, "source": s.Source, "content": s.Content}, nil
	})
}
