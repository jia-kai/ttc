package skills

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRoleForActor(t *testing.T) {
	for actor, expected := range map[string]Role{"main": Main, "main/child_A-z_09": Child, "main/btw_fixture": Aside} {
		got, err := RoleForActor(actor)
		if err != nil || got != expected {
			t.Fatal(actor, got, err)
		}
	}
	for _, actor := range []string{"", "child", "aside", "MAIN", "main/child_", "main/btw_", "main/child_a/nested", "main/btw_a b", "other/child_x"} {
		if _, err := RoleForActor(actor); err == nil {
			t.Fatal("accepted actor", actor)
		}
	}
}

func TestMetadataFrontmatter(t *testing.T) {
	for _, tc := range []struct {
		name, text, description string
		roles                   []Role
	}{
		{"plain", "# Skill\ndescription: body is not metadata\nroles: [main]\n", "", nil},
		{"later block", "# Skill\n---\nroles: [main]\n---", "", nil},
		{"empty", "---\n---\nbody", "", nil},
		{"default", "---\ndescription: 'Text: quoted'\nallowed-tools: [shell, write]\ncustom: {nested: [one, two]}\n---\nbody", "Text: quoted", nil},
		{"multiline", "---\ndescription: >-\n  useful first\n  second line\nroles:\n  - child\n  - aside\n---\nbody", "useful first second line", []Role{Child, Aside}},
		{"crlf", "---\r\nroles: [main]\r\n---\r\nbody", "", []Role{Main}},
		{"alias", "---\ncustom: &r [main]\nroles: *r\n---\nbody", "", []Role{Main}},
		{"merge", "---\ncustom: &d {roles: [main], description: merged}\n<<: *d\n---\nbody", "merged", []Role{Main}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			description, roles, err := metadata(tc.text)
			if err != nil || description != tc.description || !reflect.DeepEqual(roles, tc.roles) {
				t.Fatal(description, roles, err)
			}
		})
	}
	for _, text := range []string{
		"roles: []", "roles: null", "roles:", "roles: main", "roles: {}", "roles: [Main]", "roles: [unknown]", "roles: [main, main]", "roles: [main, 1]", "roles: [true]", "roles: [[main]]", "roles: [main]\nroles: [child]", "description: one\ndescription: two", "custom: {key: one, key: two}", "roles: [main", "[main]", "description: [not, string]", "roles: &r [main]\ncustom: *r\nroles: *r", "custom: &d {roles: []}\n<<: *d",
	} {
		t.Run(text, func(t *testing.T) {
			if _, _, err := metadata("---\n" + text + "\n---\nSECRET"); err == nil {
				t.Fatal("accepted invalid frontmatter", text)
			}
		})
	}
	if _, _, err := metadata("---\nroles: [main]\nSECRET"); err == nil {
		t.Fatal("accepted missing closing delimiter")
	}
}

func TestCatalogRolesPrecedenceAndOwnership(t *testing.T) {
	ctx := context.Background()
	project, user := t.TempDir(), t.TempDir()
	root := filepath.Join(project, ".agents", "skills")
	writeSkill(t, user, "tmux", "user fallback must never load")
	mainContent := "---\ndescription: selected metadata\nroles: [main]\nallowed-tools: [write]\n---\nSECRET selected body\n"
	path := writeSkill(t, root, "tmux", mainContent)
	writeSkill(t, root, "multi", "---\nroles: [child, aside]\n---\nshared body")
	writeSkill(t, root, "plain", "plain unchanged body\ndescription: inert")
	c, err := Discover(ctx, project, user)
	if err != nil {
		t.Fatal(err)
	}
	for actor, allowed := range map[string]bool{"main": true, "main/child_test": false, "main/btw_test": false} {
		list, err := c.List(actor)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for i, s := range list {
			if i > 0 && list[i-1].Name >= s.Name {
				t.Fatal("unsorted", list)
			}
			if s.Content != "" {
				t.Fatal("body advertised", s)
			}
			if s.Name == "tmux" {
				found = true
				if s.Source != "project" || s.Path != path {
					t.Fatal(s)
				}
				s.Roles[0] = Child
			}
		}
		if found != allowed {
			t.Fatal(actor, found, allowed)
		}
		s, err := c.Load(ctx, "tmux", actor)
		if allowed {
			if err != nil || s.Content != mainContent || s.Description != "selected metadata" {
				t.Fatal(s, err)
			}
			s.Roles[0] = Aside
		} else if err == nil || !reflect.DeepEqual(s, Skill{}) || strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("denial leaked/fell back", s, err)
		}
		multi, err := c.Load(ctx, "multi", actor)
		if actor == "main" {
			if err == nil {
				t.Fatal("main accessed multi", multi)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		plain, err := c.Load(ctx, "plain", actor)
		if err != nil || plain.Content != "plain unchanged body\ndescription: inert" || plain.Description != "" {
			t.Fatal(plain, err)
		}
	}
	for _, actor := range []string{"", "child", "main/child_"} {
		if list, err := c.List(actor); err == nil || list != nil {
			t.Fatal(list, err)
		}
		if s, err := c.Load(ctx, "plain", actor); err == nil || !reflect.DeepEqual(s, Skill{}) {
			t.Fatal(s, err)
		}
	}
	if _, err := c.Load(ctx, "TMUX", "main"); err == nil {
		t.Fatal("name matched inexactly")
	}
}

func TestCatalogCurrentFileRestrictions(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	root := filepath.Join(project, ".agents", "skills")
	path := writeSkill(t, root, "changing", "---\ndescription: snapshot\nroles: [main, child]\n---\noriginal")
	c, err := Discover(ctx, project, "")
	if err != nil {
		t.Fatal(err)
	}
	replace := func(text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	replace("---\ndescription: current\nroles: [main]\n---\nfresh")
	if s, err := c.Load(ctx, "changing", "main/child_test"); err == nil || !reflect.DeepEqual(s, Skill{}) {
		t.Fatal("revocation failed", s, err)
	}
	if s, err := c.Load(ctx, "changing", "main"); err != nil || s.Content != "---\ndescription: current\nroles: [main]\n---\nfresh" || s.Description != "snapshot" || !reflect.DeepEqual(s.Roles, []Role{Main, Child}) {
		t.Fatal("snapshot/content mismatch", s, err)
	}
	replace("new unrestricted body")
	if _, err := c.Load(ctx, "changing", "main/btw_test"); err == nil {
		t.Fatal("relaxation bypassed snapshot")
	}
	c, err = Discover(ctx, project, "")
	if err != nil {
		t.Fatal(err)
	}
	if s, err := c.Load(ctx, "changing", "main/btw_test"); err != nil || s.Content != "new unrestricted body" {
		t.Fatal("rediscovery did not relax", s, err)
	}
	for _, bad := range []string{"---\nroles: []\n---\nSECRET", "---\nroles: [aside]\nroles: [main]\n---\nSECRET", "---\nSECRET"} {
		replace(bad)
		if s, err := c.Load(ctx, "changing", "main"); err == nil || !reflect.DeepEqual(s, Skill{}) || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), path) {
			t.Fatal("invalid edit leaked/broadened", s, err)
		}
		if _, err := Discover(ctx, project, ""); err == nil {
			t.Fatal("invalid discovery broadened")
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if s, err := c.Load(ctx, "changing", "main"); err == nil || !reflect.DeepEqual(s, Skill{}) || strings.Contains(err.Error(), path) {
		t.Fatal("failed read leaked", s, err)
	}
}
