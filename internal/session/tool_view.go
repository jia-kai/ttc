package session

import (
	"context"
	"encoding/json"
	"fmt"
	"ttc/internal/jobs"
	"ttc/internal/prompts"
	"ttc/internal/render"
)

// captureJobID identifies tools returning short job previews. Reads and controls
// already carry their exact result; expanding them would change the inspected page.
func captureJobID(name string, result json.RawMessage) string {
	if name != "shell" && name != "subagent" {
		return ""
	}
	var v struct {
		ID string `json:"job_id"`
	}
	_ = json.Unmarshal(result, &v)
	return v.ID
}

const inspectionTailBytes = 8 << 10

// JobDetail expands the output tail to at most 8 KiB of each retained stream.
// An empty detail starts with current job metadata, including its command or label.
// Final records save this text; live inspectors may refresh it without growing history.
func (r *Runtime) JobDetail(id, detail string) string {
	if id == "" {
		return detail
	}
	job, err := r.Jobs.View("main", id)
	if err != nil {
		return detail + prompts.SessionJobOutputUnavailable + render.Inline(err.Error())
	}
	if detail == "" {
		key := "label"
		if job.Kind == "shell" || job.Kind == "lsp" {
			key = "command"
		}
		args, _ := json.Marshal(map[string]string{key: job.Label})
		result, _ := json.Marshal(job)
		// This is live job metadata, not the original shell arguments; don't
		// infer tool defaults such as strict mode from the retained snapshot.
		detail = render.Tool("job", args, result).Detail
	}
	streams := []string{"stdout", "stderr"}
	if job.Kind == "lsp" {
		streams = []string{"stderr"} // Protocol stdout belongs to the LSP client.
	}
	for _, stream := range streams {
		page, err := r.Jobs.Read(context.Background(), "main", id, jobs.ReadOptions{Stream: stream, Cursor: "eof:-8192:bytes", Limit: inspectionTailBytes})
		if err != nil {
			detail += fmt.Sprintf(prompts.SessionJobStreamUnavailable, stream, render.Inline(err.Error()))
			continue
		}
		text, _ := page["output"].(string)
		if text != "" {
			detail += fmt.Sprintf(prompts.SessionJobTail, stream) + render.Fence(text, "text")
		}
	}
	return detail
}
