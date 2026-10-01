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

// JobDetail expands the output tail to the last 64 KiB of each retained stream.
// Final records save this text; live inspectors may refresh it without growing history.
func (r *Runtime) JobDetail(id, detail string) string {
	if id == "" {
		return detail
	}
	for _, stream := range []string{"stdout", "stderr"} {
		page, err := r.Jobs.Read(context.Background(), "main", id, jobs.ReadOptions{Stream: stream, Cursor: "eof:-65536:bytes", Limit: 65536})
		if err != nil {
			return detail + "\nOutput unavailable: " + render.Inline(err.Error())
		}
		text, _ := page["output"].(string)
		if text != "" {
			detail += "\n**" + stream + " · last 64 KiB retained**\n\n" + render.Fence(text, "text")
		}
	}
	return detail
}
