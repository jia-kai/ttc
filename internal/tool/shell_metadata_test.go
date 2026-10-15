package tool

import (
	"context"
	"strings"
	"testing"

	"ttc/internal/jobs"
)

func TestShellLabelAndDetailedJobList(t *testing.T) {
	r, w, x, req := toolFixture(t)
	m := jobs.New(context.Background(), nil)
	defer m.Close()
	AddShell(r, m, w, nil)
	invoke(t, r, w, x, req, "shell", `{"command":"printf retained-marker","label":"Quick check"}`)
	for _, details := range []bool{false, true} {
		args := `{"state":"all"}`
		if details {
			args = `{"state":"all","details":true}`
		}
		record := invoke(t, r, w, x, req, "job_list", args)
		result := string(record.Result)
		if !strings.Contains(result, "Quick check") || strings.Contains(result, `"stdout"`) || strings.Contains(result, "printf retained-marker") != details {
			t.Fatal(result)
		}
	}
}
