package render

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestJobReference(t *testing.T) {
	for _, tc := range []struct{ kind, label, id, want string }{
		{"shell", "Run tests", "job_exact", "shell=Run tests"},
		{"child", "Review", "child_exact", "child=Review"},
		{"watch", "", "job_exact", "watch=job_exact"},
		{"", "", "job_exact", "job=job_exact"},
		{"lsp", " \n\t", "job_exact", "lsp=job_exact"},
		{"shell", "  Run\n\ttests\x1b\u202e ", "job_exact", "shell=Run tests"},
		{"shell", "*[x]`", "job_exact", "shell=*[x]`"},
		{"shell", "", "", ""},
	} {
		if got := JobReference(tc.kind, tc.label, tc.id); got != tc.want {
			t.Errorf("JobReference(%q, %q, %q) = %q, want %q", tc.kind, tc.label, tc.id, got, tc.want)
		}
	}
}

func TestJobReferenceBounds(t *testing.T) {
	got := JobReference(strings.Repeat("界", 100), strings.Repeat("界", 400), "exact")
	if !utf8.ValidString(got) || len(got) > 199 || !strings.Contains(got, "…=") || !strings.HasSuffix(got, "…") {
		t.Fatal(got)
	}
}
