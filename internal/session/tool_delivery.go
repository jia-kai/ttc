package session

import (
	"encoding/json"
	"fmt"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/prompts"
	"ttc/internal/render"
	"ttc/internal/tool"
)

// toolResultBudget bounds original result payloads delivered by one request's
// tool batch. It depends only on the producing model's capacity, never on free
// space in the current conversation. The batch owner allocates it in completion
// order; small replacement notices are additional framing, not original payload.
type toolResultBudget struct {
	limit, used int
}

func (b *toolResultBudget) deliver(r *Runtime, session string, original tool.Record) (tool.Record, error) {
	tokens := contextbuild.Tokens([]llm.Message{{Type: llm.TypeToolResult, Content: string(original.Result), Files: original.Files}})
	if len(original.Result) <= 64<<10 && tokens < b.limit-b.used {
		b.used += tokens
		return original, nil
	}
	// Execution has already happened. Keep the exact original in the immutable
	// tool record and a directly readable artifact; only coding input is replaced.
	path, err := r.Store.Artifact(session, "details", original.Result)
	var retentionErr error
	if err != nil {
		retentionErr = fmt.Errorf(prompts.SessionToolResultRetention, err)
	}
	message := prompts.SessionToolResultBudget
	if len(original.Result) > 64<<10 {
		message = prompts.SessionToolResultByteLimit
	}
	value := map[string]any{
		"ok": false, "truncated": true,
		"error":         tool.Fail("result_too_large", message),
		"result_tokens": tokens, "batch_limit_tokens": b.limit,
	}
	if path != "" {
		value["detail_path"] = path
	}
	// Retain routing/status facts without copying potentially large output. This
	// lets the model inspect a completed command rather than executing it twice.
	var fields struct {
		JobID            json.RawMessage `json:"job_id"`
		ChildID          json.RawMessage `json:"child_id"`
		Status           json.RawMessage `json:"status"`
		ExitCode         json.RawMessage `json:"exit_code"`
		Signal           json.RawMessage `json:"signal"`
		AssignmentStatus json.RawMessage `json:"assignment_status"`
		ChildState       json.RawMessage `json:"child_state"`
		Warnings         json.RawMessage `json:"warnings"`
	}
	if json.Unmarshal(original.Result, &fields) == nil {
		if len(fields.Warnings) > 0 && len(fields.Warnings) <= 8192 {
			value["warnings"] = fields.Warnings
		}
		for key, field := range map[string]json.RawMessage{
			"job_id": fields.JobID, "child_id": fields.ChildID, "status": fields.Status,
			"exit_code": fields.ExitCode, "signal": fields.Signal,
			"assignment_status": fields.AssignmentStatus, "child_state": fields.ChildState,
		} {
			if len(field) > 0 && len(field) <= 256 {
				value[key] = field
			}
		}
	}
	result, err := json.Marshal(value)
	if err != nil {
		return original, err
	}
	delivered := original
	delivered.Result, delivered.Files = result, nil
	delivered.Markdown = render.Tool(original.Name, original.Arguments, result)
	// The presentation explains withholding while retaining exact execution
	// evidence in its inspector. The original Result/Files remain in storage.
	delivered.Markdown.Detail += "\n\n" + original.Markdown.Detail
	return delivered, retentionErr
}
