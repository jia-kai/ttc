package skills

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMetadataRejectsTrailingYAML(t *testing.T) {
	for _, tail := range []string{"roles: [main]", "[invalid", "%YAML 1.2"} {
		content := "---\ndescription: first\n...\n" + tail + "\n---\nPRIVATE body"
		if _, _, err := metadata(content); err == nil {
			t.Fatalf("accepted trailing YAML %q", tail)
		}
	}
	for _, content := range []string{"---\n---\nbody", "---\n# comment\n---\nbody", "---\nroles: [main]\n...\n# comment\n---\nbody"} {
		if _, _, err := metadata(content); err != nil {
			t.Fatalf("rejected single document %q: %v", content, err)
		}
	}
}

func TestCatalogRejectsTrailingYAMLAtDiscoveryAndLoad(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, ".agents", "skills", "trailing", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("plain instructions")
	catalog, err := Discover(ctx, root, "")
	if err != nil {
		t.Fatal(err)
	}
	write("---\ndescription: first\n...\nroles: [main]\n---\nPRIVATE body")
	if _, err := Discover(ctx, root, ""); err == nil {
		t.Fatal("discovery accepted malformed frontmatter")
	}
	loaded, err := catalog.Load(ctx, "trailing", "main/btw_review")
	if err == nil || loaded.Content != "" || loaded.Path != "" {
		t.Fatalf("load accepted or exposed malformed source: %+v, %v", loaded, err)
	}
}
