package render

import (
	"strings"
	"testing"
)

func TestToolParametersReadablePreciseAndSafe(t *testing.T) {
	md := Tool("question", []byte(`{"questions":[{"question":"Which?","options":["one","two"],"recommendation":"one"}]}`), []byte(`{"ok":true,"large":9007199254740993}`))
	for _, want := range []string{"**Questions:**", "**Question:**", "**Options:**", "**Recommendation:**", "9007199254740993"} {
		if !strings.Contains(md.Detail, want) {
			t.Fatal("missing", want, md.Detail)
		}
	}
	if strings.Contains(md.Detail, `"questions":`) || strings.Contains(md.Detail, `"large":`) {
		t.Fatal("primary JSON presentation", md.Detail)
	}
	md = Tool("shell", []byte("{\"command\":\"printf '```'\\necho \\u001b[31m\"}"), nil)
	if !strings.Contains(md.Detail, "**Strict:** true") || !strings.Contains(md.Detail, "````sh") || strings.ContainsRune(md.Detail, '\x1b') {
		t.Fatal(md.Detail)
	}
	if DiffBriefing(md.Summary) {
		t.Fatal("shell command treated as a multiline diff")
	}
	if !DiffBriefing("**write** · path\n\n" + Fence("+new", "diff")) {
		t.Fatal("short diff unrecognized")
	}
}
