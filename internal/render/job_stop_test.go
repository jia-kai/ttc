package render

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestJobStopSummaries(t *testing.T) {
	for _, tc := range []struct{ name, args, result, want string }{
		{"child", `{"child_id":"child_exact"}`, `{"label":"Review code","child_state":"closed","assignment_status":"completed"}`, "**job_stop(child=Review code)** · closed"},
		{"shell", `{"job_id":"job_exact"}`, `{"kind":"shell","label":"Run tests","status":"cancelled"}`, "**job_stop(shell=Run tests)** · cancelled"},
		{"watch", `{"job_id":"job_exact"}`, `{"kind":"watch","label":"Training","status":"cancelled"}`, "**job_stop(watch=Training)** · cancelled"},
		{"lsp", `{"job_id":"job_exact"}`, `{"kind":"lsp","label":"gopls","status":"completed"}`, "**job_stop(lsp=gopls)** · done"},
		{"pending", `{"job_id":"job_exact"}`, `null`, "**job_stop(job=job\\_exact)**"},
		{"pending child", `{"child_id":"child_exact"}`, `null`, "**job_stop(child=child\\_exact)**"},
		{"error", `{"job_id":"missing"}`, `{"error":{"message":"not found"}}`, "**job_stop(job=missing)** · error: not found"},
		{"empty", `{}`, `null`, "**job_stop**"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := Tool("job_stop", []byte(tc.args), []byte(tc.result))
			if md.Summary != tc.want {
				t.Fatalf("summary = %q, want %q", md.Summary, tc.want)
			}
			if strings.Contains(tc.args, "job_exact") && !strings.Contains(md.Detail, "job_exact") {
				t.Fatal(md.Detail)
			}
		})
	}
}

func TestJobStopSummaryEscapingAndBounds(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"kind": "shell", "label": " \x1b\u202e*[x]`\n" + strings.Repeat("界", 400), "stdout": "must not appear", "status": "cancelled"})
	md := Tool("job_stop", []byte(`{"job_id":"exact"}`), raw)
	if !utf8.ValidString(md.Summary) || len(md.Summary) > 260 || strings.ContainsAny(md.Summary, "\x1b\u202e\n") || strings.Contains(md.Summary, "must not appear") {
		t.Fatal(md.Summary)
	}
	if !strings.Contains(md.Summary, "\\*\\[x\\]\\`") {
		t.Fatal(md.Summary)
	}
}
