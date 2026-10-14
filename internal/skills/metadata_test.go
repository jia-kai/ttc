package skills

import (
	"fmt"
	"strings"
	"testing"

	"ttc/internal/prompts"
)

func TestMetadataComplexityLimits(t *testing.T) {
	mapping := func(pairs int) string {
		var text strings.Builder
		for i := 0; i < pairs; i++ {
			fmt.Fprintf(&text, "key%d: value\n", i)
		}
		return strings.TrimSuffix(text.String(), "\n")
	}
	sequence := func(items int) string {
		return "custom: [" + strings.TrimSuffix(strings.Repeat("x,", items), ",") + "]"
	}
	for _, tc := range []struct {
		name, yaml string
		valid      bool
	}{
		{"mapping at limit", mapping(maxFrontmatterMappingPairs), true},
		{"mapping over limit", mapping(maxFrontmatterMappingPairs + 1), false},
		{"nested mapping at limit", "custom:\n" + indentMetadata(mapping(maxFrontmatterMappingPairs)), true},
		{"nested mapping over limit", "custom:\n" + indentMetadata(mapping(maxFrontmatterMappingPairs+1)), false},
		{"sequence mapping over limit", "custom:\n  -\n" + indentMetadata(indentMetadata(mapping(maxFrontmatterMappingPairs+1))), false},
		// The root, key, and sequence consume three nodes before the items.
		{"nodes at limit", sequence(maxFrontmatterNodes - 3), true},
		{"nodes over limit", sequence(maxFrontmatterNodes - 2), false},
		{"repeated mappings below node limit", "custom:\n" + strings.Repeat("  -\n"+indentMetadata(indentMetadata(mapping(maxFrontmatterMappingPairs)))+"\n", 7), true},
		{"repeated mappings over node limit", "custom:\n" + strings.Repeat("  -\n"+indentMetadata(indentMetadata(mapping(maxFrontmatterMappingPairs)))+"\n", 8), false},
		{"bytes at limit", "#" + strings.Repeat("x", maxFrontmatterBytes-1), true},
		{"bytes over limit", "#" + strings.Repeat("x", maxFrontmatterBytes), false},
		{"many comment lines over byte limit", strings.Repeat("#\n", maxFrontmatterBytes/2+1), false},
		{"oversized alias target", "custom: &base\n" + indentMetadata(mapping(maxFrontmatterMappingPairs+1)) + "\n<<: *base", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, closing := range []string{"---", "---\nbody", "---\r\nbody"} {
				_, _, err := metadata("---\n" + tc.yaml + "\n" + closing)
				if tc.valid {
					if err != nil {
						t.Fatal(err)
					}
				} else if want := fmt.Sprintf(prompts.SkillFrontmatterTooLarge, maxFrontmatterBytes, maxFrontmatterMappingPairs, maxFrontmatterNodes); err == nil || err.Error() != want {
					t.Fatalf("got %v, want %s", err, want)
				}
			}
		})
	}
}

func indentMetadata(text string) string {
	return "  " + strings.ReplaceAll(text, "\n", "\n  ")
}

func TestMetadataOnlyScansOpeningFrontmatter(t *testing.T) {
	body := strings.Repeat("plain Markdown\n", maxFrontmatterBytes)
	for _, text := range []string{body, "\ufeff" + body} {
		if allocs := testing.AllocsPerRun(10, func() {
			if description, roles, err := metadata(text); err != nil || description != "" || roles != nil {
				t.Fatal(description, roles, err)
			}
		}); allocs != 0 {
			t.Fatalf("plain Markdown allocated %g times", allocs)
		}
	}
	for _, prefix := range []string{"---\n", "\ufeff---\n", "---\r\n"} {
		if description, roles, err := metadata(prefix + "description: intact\n---\n" + body); err != nil || description != "intact" || roles != nil {
			t.Fatal(description, roles, err)
		}
	}
	if _, _, err := metadata("---"); err == nil {
		t.Fatal("accepted opening delimiter without closing delimiter")
	}
}

func TestMetadataAliasLimitsDoNotFollowTargets(t *testing.T) {
	// Alias syntax counts once, even when the shared target is used repeatedly.
	text := "---\ncustom: &items [" + strings.TrimSuffix(strings.Repeat("x,", 128), ",") + "]\nrefs: [" + strings.TrimSuffix(strings.Repeat("*items,", 16), ",") + "]\nbase: &base {roles: [child], description: merged}\n<<: *base\n---\nbody"
	if description, roles, err := metadata(text); err != nil || description != "merged" || len(roles) != 1 || roles[0] != Child {
		t.Fatal(description, roles, err)
	}
	if _, _, err := metadata("---\ncustom: &loop [*loop]\n---\nbody"); err == nil {
		t.Fatal("accepted cyclic alias")
	}
}

func BenchmarkMetadataLargeMarkdown(b *testing.B) {
	body := strings.Repeat("plain Markdown\n", maxFrontmatterBytes)
	for _, tc := range []struct{ name, text string }{
		{"plain", body},
		{"frontmatter", "---\ndescription: short\nroles: [main]\n---\n" + body},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := metadata(tc.text); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
