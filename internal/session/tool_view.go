package session

import (
	"context"
	"encoding/json"
	"scicode/internal/jobs"
	"scicode/internal/render"
)

func resultJobID(result json.RawMessage) string {
	var v struct {
		ID string `json:"job_id"`
	}
	_ = json.Unmarshal(result, &v)
	return v.ID
}

const inspectionTailBytes = 8 << 10

// JobDetail expands the output tail to at most 8 KiB of each retained stream.
// Final records save this text; live inspectors may refresh it without growing history.
func (r *Runtime) JobDetail(id, detail string) string {
	if id == "" {
		return detail
	}
	job, err := r.Jobs.View("main", id)
	if err != nil {
		return detail + "\nOutput unavailable: " + render.Inline(err.Error())
	}
	streams := []string{"stdout", "stderr"}
	if job.Kind == "lsp" {
		streams = []string{"stderr"} // Protocol stdout belongs to the LSP client.
	}
	for _, stream := range streams {
		page, err := r.Jobs.Read(context.Background(), "main", id, jobs.ReadOptions{Stream: stream, Cursor: "eof:-8192:bytes", Limit: inspectionTailBytes})
		if err != nil {
			detail += "\n" + stream + " unavailable: " + render.Inline(err.Error())
			continue
		}
		text, _ := page["output"].(string)
		if text != "" {
			detail += "\n**" + stream + " · showing tail, up to 8 KiB**\n\n" + render.Fence(text, "text")
		}
	}
	return detail
}
