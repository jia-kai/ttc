package jobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"ttc/internal/capture"
)

func TestCompactLabelsAndExplicitCommandInspection(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	command := "printf 'retained output'; printf 'diagnostics' >&2"
	label := "  Check\n " + strings.Repeat("界", 100)
	id, err := m.Start("main/child", command, t.TempDir(), 0, true, false, capture.OutputOptions{}, label)
	if err != nil {
		t.Fatal(err)
	}
	view, err := m.Wait(context.Background(), "main", id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if utf8.RuneCountInString(view.Label) != 80 || strings.ContainsAny(view.Label, "\n\t") {
		t.Fatal(view.Label)
	}
	if view.Stdout != "retained output" {
		t.Fatal(view)
	}
	for _, details := range []bool{false, true} {
		list := m.ListMetadata("main", true, details)
		if len(list) != 1 {
			t.Fatal(list)
		}
		data, err := json.Marshal(list)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "retained output") != details || strings.Contains(string(data), `"stdout"`) || strings.Contains(string(data), `"stderr"`) {
			t.Fatal(string(data))
		}
		if details && list[0].Command != command {
			t.Fatal(list)
		}
	}
	if got := m.ListMetadata("main/unrelated", true, true); len(got) != 0 {
		t.Fatal(got)
	}
	inspection, err := m.Inspect(context.Background(), "main", id)
	if err != nil || inspection.Spec.Fields[0].Value != command {
		t.Fatal(inspection, err)
	}
	if got := compactLabel("", "Shell command"); got != "Shell command" {
		t.Fatal(got)
	}
}

func TestCompactLabelNormalizesControls(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"\n\t\x00\x1b\u0085", "fallback"},
		{"  check\nresults\t now  ", "check results now"},
		{"check\x1b[31m\x00results\u009b", "check [31m results"},
	} {
		if got := compactLabel(tc.input, "fallback"); got != tc.want {
			t.Errorf("compactLabel(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
