package skills

import (
	"fmt"
	"strings"

	"ttc/internal/prompts"
)

// Role is a runtime skill audience, not a tool permission or sandbox policy.
type Role string

const (
	// Main is the user-facing coding agent.
	Main Role = "main"
	// Child is a delegated coding agent.
	Child Role = "child"
	// Aside is a read-only aside agent.
	Aside Role = "aside"
)

// RoleForActor classifies runtime actor IDs. Unknown or malformed IDs fail closed.
func RoleForActor(actor string) (Role, error) {
	if actor == "main" {
		return Main, nil
	}
	for _, entry := range []struct {
		prefix string
		role   Role
	}{{"main/child_", Child}, {"main/btw_", Aside}} {
		if !strings.HasPrefix(actor, entry.prefix) {
			continue
		}
		suffix := strings.TrimPrefix(actor, entry.prefix)
		if suffix == "" {
			break
		}
		for _, r := range suffix {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				return "", fmt.Errorf(prompts.SkillInvalidActor)
			}
		}
		return entry.role, nil
	}
	return "", fmt.Errorf(prompts.SkillInvalidActor)
}

func allows(roles []Role, role Role) bool {
	if len(roles) == 0 {
		return true
	}
	for _, candidate := range roles {
		if candidate == role {
			return true
		}
	}
	return false
}
