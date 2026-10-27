package session

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"ttc/internal/jobs"
	"ttc/internal/render"
)

func TestJobInspectionMetadataStaysCompact(t *testing.T) {
	job := jobs.Snapshot{
		ID: "job_fixture", Kind: "shell", Owner: "main", Label: "shell fixture",
		Status: "completed", StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:00:01Z",
	}
	for _, fields := range [][]jobs.InspectionField{
		{{Name: "Command", Value: "printf 'hello'", Language: "sh"}},
		{{Name: "Label", Value: "shell fixture"}},
		{{Name: "Command", Value: "echo first\necho second", Language: "sh"}, {Name: "Record dir", Value: "/runs/fixture"}},
	} {
		for _, invocation := range []bool{false, true} {
			for _, width := range []int{32, 100} {
				t.Run(fmt.Sprintf("%s/invocation=%v/width=%d", fields[0].Name, invocation, width), func(t *testing.T) {
					base := ""
					if invocation {
						result, err := json.Marshal(job)
						if err != nil {
							t.Fatal(err)
						}
						base = render.InspectedTool("shell", []byte(`{"command":"printf 'hello'"}`), result).Detail
					}
					detail := jobInspectionDetail(jobs.Inspection{Job: job, Spec: jobs.InspectionSpec{Fields: fields}}, base, time.Time{})
					text, err := render.Terminal(detail, width)
					if err != nil {
						t.Fatal(err)
					}
					plain := ansi.Strip(text)
					rows := strings.Split(plain, "\n")
					// This metadata slice is short enough not to wrap even in a
					// narrow pane. A blank separator must not loosen the list.
					start, end := -1, -1
					for i, row := range rows {
						if strings.Contains(row, "Job id:") {
							start = i
						}
						if strings.Contains(row, "Kind:") {
							end = i
						}
					}
					if start < 0 || end != start+1 {
						t.Fatalf("job metadata has extra rows (id=%d, kind=%d):\n%s", start, end, plain)
					}
					for _, field := range fields {
						if !strings.Contains(plain, field.Name+":") {
							t.Fatalf("inspection lost field %q:\n%s", field.Name, plain)
						}
						for _, line := range strings.Split(field.Value, "\n") {
							if !strings.Contains(plain, line) {
								t.Fatalf("inspection lost field content %q:\n%s", line, plain)
							}
						}
					}
				})
			}
		}
	}
}
