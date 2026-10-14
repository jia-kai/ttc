package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/skills"
)

func TestSkillToolUsesExecutionActorAndHidesDeniedContent(t *testing.T) {
	project := t.TempDir()
	path := filepath.Join(project, ".agents", "skills", "restricted", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	content := "---\nroles: [main]\n---\nPRIVATE instructions\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := skills.Discover(context.Background(), project, "")
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	AddSkills(r, c)
	for _, actor := range []string{"main", "main/child_test", "main/btw_test", "", "child"} {
		record := r.Invoke(context.Background(), Execution{Actor: actor}, "skill", []byte(`{"name":"restricted"}`))
		var output struct {
			OK      bool   `json:"ok"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(record.Result, &output); err != nil {
			t.Fatal(err)
		}
		if actor == "main" {
			if !output.OK || output.Content != content {
				t.Fatal(string(record.Result))
			}
		} else if output.OK || strings.Contains(string(record.Result), "PRIVATE") || strings.Contains(string(record.Result), path) || !strings.Contains(string(record.Result), "skill_unavailable") {
			t.Fatal(actor, string(record.Result))
		}
	}
	// An unrestricted document still requires a recognized runtime actor.
	if record := r.Invoke(context.Background(), Execution{}, "skill", []byte(`{"name":"lsp"}`)); strings.Contains(string(record.Result), `"ok":true`) || !strings.Contains(string(record.Result), "valid runtime actor") {
		t.Fatal(string(record.Result))
	}
}
