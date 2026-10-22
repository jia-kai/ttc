package prompts

import (
	"strings"
	"testing"
)

func TestCompactionMarkdownParagraphBreaks(t *testing.T) {
	for _, tc := range []struct {
		name, text, boundary string
	}{
		{"Compaction", Compaction, "\n\nKeep the handoff"},
		{"CompactionContext", CompactionContext, "\n\n%s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.text, tc.boundary) {
				t.Fatalf("Markdown paragraph break missing before %q", tc.boundary)
			}
		})
	}
	for _, heading := range []string{"Goal and constraints", "Decisions", "Progress and evidence", "Next steps"} {
		if !strings.Contains(Compaction, "\n\n## "+heading+"\n\n") {
			t.Errorf("compaction heading %q must be surrounded by Markdown blank lines", heading)
		}
	}
}
