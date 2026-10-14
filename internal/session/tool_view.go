package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"ttc/internal/jobs"
	"ttc/internal/render"
)

// captureJobID identifies tools returning short job previews. Reads and controls
// already carry their exact result; expanding them would change the inspected page.
func captureJobID(name string, result json.RawMessage) string {
	if name != "shell" && name != "subagent" && name != "watch" {
		return ""
	}
	var v struct {
		ID string `json:"job_id"`
	}
	_ = json.Unmarshal(result, &v)
	return v.ID
}

// JobDetail renders one typed inspection with bounded output and common runtime
// metadata. detail preserves the original tool invocation when supplied. The same
// renderer serves live windows and immutable completion records; history never
// invokes the job's customization callback.
func (r *Runtime) JobDetail(id, detail string) string {
	if id == "" {
		return detail
	}
	inspection, err := r.Jobs.Inspect(context.Background(), "main", id)
	if err != nil {
		return detail + "\nOutput unavailable: " + render.Inline(err.Error())
	}
	return jobInspectionDetail(inspection, detail, time.Now())
}

func jobInspectionDetail(inspection jobs.Inspection, detail string, now time.Time) string {
	job := inspection.Job
	if detail == "" {
		result, _ := json.Marshal(job)
		// This is current metadata, not a fabricated original tool invocation.
		detail = "Job:\n\n" + render.JobValues(result)
	}
	var out strings.Builder
	if inspection.Spec.Title != "" {
		out.WriteString("## " + render.EscapeInline(inspection.Spec.Title) + "\n\n")
	}
	out.WriteString(detail)
	out.WriteString("\n")
	for _, field := range inspection.Spec.Fields {
		out.WriteString("- **" + render.EscapeInline(field.Name) + ":** ")
		if field.Language != "" || strings.ContainsAny(field.Value, "\n\t") {
			out.WriteString("\n\n" + render.Fence(field.Value, field.Language) + "\n")
		} else {
			out.WriteString(render.EscapeInline(field.Value) + "\n")
		}
	}
	if duration, ok := jobRunningTime(job, now); ok {
		fmt.Fprintf(&out, "\n**Running time:** %s\n", duration)
	}
	for _, stream := range inspection.Output {
		if stream.Error != "" {
			fmt.Fprintf(&out, "\n%s unavailable: %s\n", render.EscapeInline(stream.Name), render.Inline(stream.Error))
			continue
		}
		if stream.Text == "" && !stream.Truncated {
			continue
		}
		if stream.Heading != "" {
			fmt.Fprintf(&out, "\n**%s**\n\n", render.EscapeInline(stream.Heading))
		} else if !stream.Markdown {
			fmt.Fprintf(&out, "\n**%s · showing tail, up to %d KiB**\n\n", render.EscapeInline(stream.Name), stream.LimitBytes/1024)
		}
		if stream.Truncated {
			fmt.Fprintf(&out, "\nRetained %s output was truncated.\n\n", render.EscapeInline(stream.Name))
		}
		if stream.Text == "" {
			continue // Disclose wholly evicted captures without inventing output.
		}
		if stream.Markdown {
			out.WriteString(render.Clean(stream.Text) + "\n")
		} else {
			out.WriteString(render.Fence(stream.Text, "text"))
		}
	}
	return out.String()
}

// jobRunningTime measures the job (the observer for watches), not an external
// workload. Finished jobs stay fixed; seconds are truncated and hours do not wrap.
func jobRunningTime(job jobs.Snapshot, now time.Time) (string, bool) {
	started, err := time.Parse(time.RFC3339Nano, job.StartedAt)
	if err != nil {
		return "", false
	}
	end := now
	if job.Status != "running" {
		end, err = time.Parse(time.RFC3339Nano, job.FinishedAt)
		if err != nil {
			return "", false
		}
	}
	seconds := max(int64(0), int64(end.Sub(started)/time.Second))
	return fmt.Sprintf("%dh%dm%ds", seconds/3600, seconds/60%60, seconds%60), true
}
