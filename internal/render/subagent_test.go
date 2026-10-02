package render

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

func TestSubagentBadgeNameAndCellBounds(t *testing.T) {
	for _, name := range []string{"research", strings.Repeat("x", 26), strings.Repeat("界", 27), "one\x1btwo\nthree"} {
		full := SubagentBadge("child-a", name, 100, false)
		if strings.ContainsAny(full, "\x1b\n") || !utf8.ValidString(full) {
			t.Fatal(full)
		}
		if utf8.RuneCountInString(name) == 27 && full != "[Sub "+strings.Repeat("界", 23)+"...]" {
			t.Fatal("name truncation", full)
		}
		for _, width := range []int{1, 2, 8, 26, 80} {
			colored := SubagentBadge("child-a", name, width, true)
			plain := SubagentBadge("child-a", name, width, false)
			if ansi.Strip(colored) != plain || ansi.StringWidth(colored) > width || !utf8.ValidString(colored) {
				t.Fatal(width, colored, plain)
			}
			if colored != SubagentBadge("child-a", name, width, true) || !strings.Contains(colored, "\x1b[38;2;") {
				t.Fatal("unstable or absent color", colored)
			}
		}
	}
}

func TestSuccessfulToolStatusPresentation(t *testing.T) {
	md := Tool("shell", []byte(`{"command":"true"}`), []byte(`{"status":"completed","exit_code":0}`))
	if !strings.Contains(md.Summary, "done") || !strings.Contains(md.Detail, "done") || strings.Contains(md.Detail, "completed") {
		t.Fatal(md)
	}
	for _, status := range []string{"running", "failed", "cancelled", "interrupted"} {
		if Status(status) != status {
			t.Fatal(status)
		}
	}
}
