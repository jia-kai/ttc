package skills

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedAndPrecedence(t *testing.T) {
	project, user := t.TempDir(), t.TempDir()
	c, e := Discover(context.Background(), project, user)
	if e != nil {
		t.Fatal(e)
	}
	s, e := c.Load(context.Background(), "tmux")
	if e != nil || s.Source != "bundled" || s.Content == "" {
		t.Fatalf("%+v %v", s, e)
	}
	for _, root := range []string{user, filepath.Join(project, ".agents/skills")} {
		os.MkdirAll(filepath.Join(root, "tmux"), 0700)
		os.WriteFile(filepath.Join(root, "tmux/SKILL.md"), []byte(root), 0600)
	}
	c, e = Discover(context.Background(), project, user)
	if e != nil {
		t.Fatal(e)
	}
	s, e = c.Load(context.Background(), "tmux")
	if e != nil || s.Source != "project" {
		t.Fatal(s, e)
	}
	if _, e = c.Load(context.Background(), "../missing"); e == nil {
		t.Fatal("accepted unknown skill")
	}
}

func TestAncestorAndSingularSkills(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "repo", "nested")
	user := filepath.Join(t.TempDir(), ".agents", "skills")
	write := func(directory, name, content string) {
		t.Helper()
		path := filepath.Join(directory, name, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("---\ndescription: chosen layer\n---\n"+content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(filepath.Dir(user), "skill"), "user-only", "singular user")
	write(user, "lsp", "user")
	write(filepath.Join(root, "repo", ".agents", "skills"), "lsp", "parent")
	write(filepath.Join(root, "repo", ".agents", "skills"), "inherited", "ancestor skill")
	write(filepath.Join(project, ".agents", "skill"), "lsp", "singular cwd")
	write(filepath.Join(project, ".agents", "skills"), "lsp", "plural cwd")
	write(filepath.Join(project, ".agents", "skill"), "singular-only", "singular project")
	catalog, err := Discover(context.Background(), project, user)
	if err != nil {
		t.Fatal(err)
	}
	for name, expected := range map[string]string{
		"lsp": "plural cwd", "inherited": "ancestor skill",
		"singular-only": "singular project", "user-only": "singular user",
	} {
		skill, err := catalog.Load(context.Background(), name)
		if err != nil || skill.Content != "---\ndescription: chosen layer\n---\n"+expected || skill.Description != "chosen layer" {
			t.Fatalf("%s: %+v, %v", name, skill, err)
		}
		if name != "user-only" && skill.Source != "project" {
			t.Fatalf("ancestor skill not marked as project: %+v", skill)
		}
	}
	if err := os.Remove(filepath.Join(project, ".agents", "skills", "lsp", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	catalog, err = Discover(context.Background(), project, user)
	if err != nil {
		t.Fatal(err)
	}
	skill, err := catalog.Load(context.Background(), "lsp")
	if err != nil || skill.Path != filepath.Join(project, ".agents", "skill", "lsp", "SKILL.md") {
		t.Fatal("singular cwd did not override parent/user", skill, err)
	}
}
