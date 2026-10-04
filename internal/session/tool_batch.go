package session

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"ttc/internal/provider"
	"ttc/internal/render"
	"ttc/internal/tool"
)

func fileMutation(name string) bool {
	return name == "write" || name == "edit" || name == "patch"
}

func readOnlyTool(name string) bool {
	switch name {
	case "read", "glob", "grep", "skill", "web_fetch", "web_search", "job_list", "job_read", "wakeup_list", "lsp_query":
		return true
	}
	return false
}

func failedTool(call provider.ToolCall, code, message string) tool.Record {
	result, _ := json.Marshal(map[string]any{"ok": false, "error": tool.Fail(code, message)})
	return tool.Record{Name: call.Name, Arguments: call.Arguments, Result: result, Markdown: render.Tool(call.Name, call.Arguments, result)}
}

// runToolBatch settles every emitted call once. Reads run concurrently before
// ordered writes/control calls; shells and children overlap both phases. The
// caller drains completions and persists them as they arrive. Returned records
// retain model call order, independent of completion order. Routing locks cover
// child file mutations/commits so compaction can advance their continuation.
func (r *Runtime) runToolBatch(ctx context.Context, turn, actor string, registry *tool.Registry, calls []provider.ToolCall, ids []string, streamErr error) ([]tool.Record, error) {
	if len(calls) != len(ids) {
		return nil, errors.New("tool call/intent count mismatch")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type completion struct {
		index  int
		record tool.Record
		stored chan struct{}
	}
	done := make(chan completion)
	entryIDs := make([]int64, len(ids))
	requestIDs := make([]int64, len(ids))
	binaryFiles := make([][]provider.BinaryFileType, len(ids))
	for i, id := range ids {
		// Call IDs are globally unique. A continuation may archive rather than copy
		// this intent; its historical entry is still inspectable while the call runs.
		var modelJSON string
		if err := r.Store.DB.QueryRow("SELECT e.id,c.request_id,m.model_json FROM entries e JOIN tool_calls c ON c.id=json_extract(e.content_json,'$.call_id') JOIN model_requests m ON m.id=c.request_id WHERE e.kind='tool_call' AND c.id=? ORDER BY e.id DESC LIMIT 1", id).Scan(&entryIDs[i], &requestIDs[i], &modelJSON); err != nil {
			return nil, err
		}
		var selection provider.Selection
		if err := json.Unmarshal([]byte(modelJSON), &selection); err != nil {
			return nil, err
		}
		binaryFiles[i] = selection.Model.BinaryFileTypes()
	}
	run := func(i int) {
		call := calls[i]
		var record tool.Record
		if streamErr != nil {
			record = failedTool(call, "interrupted", "stream did not complete; tool was not executed")
		} else if err := ctx.Err(); err != nil {
			record = failedTool(call, "cancelled", err.Error())
		} else {
			if fileMutation(call.Name) {
				r.routeMu.RLock()
			}
			update := func(value any) {
				result, err := json.Marshal(value)
				if err != nil {
					return
				}
				md := render.Tool(call.Name, call.Arguments, result)
				text := md.Summary
				r.emit(Event{Kind: "tool_update", Actor: actor, CallID: ids[i], PendingKey: pendingToolKey(requestIDs[i], call.ID), JobID: captureJobID(call.Name, result), EntryID: entryIDs[i], Text: text, Detail: md.Detail})
			}
			update(map[string]string{"status": "running"})
			record = registry.Invoke(ctx, tool.Execution{SessionID: r.Current(), CallID: ids[i], Actor: actor, Update: update, BinaryFiles: binaryFiles[i]}, call.Name, call.Arguments)
			if fileMutation(call.Name) {
				r.routeMu.RUnlock()
			}
		}
		record.Markdown.Detail = r.JobDetail(captureJobID(call.Name, record.Result), record.Markdown.Detail)
		stored := make(chan struct{})
		done <- completion{i, record, stored}
		<-stored // Finish persistence before admitting the next ordered call.
	}
	var reads sync.WaitGroup
	var workers sync.WaitGroup
	for i, call := range calls {
		if readOnlyTool(call.Name) {
			reads.Add(1)
			workers.Add(1)
			go func() { defer workers.Done(); defer reads.Done(); run(i) }()
		} else if call.Name == "shell" || call.Name == "subagent" {
			workers.Add(1)
			go func() { defer workers.Done(); run(i) }()
		}
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		reads.Wait()
		for i, call := range calls {
			if !readOnlyTool(call.Name) && call.Name != "shell" && call.Name != "subagent" {
				run(i)
			}
		}
	}()
	records := make([]tool.Record, len(calls))
	var batchErr error
	for range calls {
		result := <-done
		records[result.index] = result.record
		r.routeMu.RLock()
		entry, err := r.Store.CallResult(r.Current(), turn, actor, ids[result.index], result.record.Result, result.record.Files, result.record, result.record.Markdown, actor == "main")
		r.routeMu.RUnlock()
		if err != nil {
			batchErr = errors.Join(batchErr, err)
			cancel()
			close(result.stored)
			continue
		}
		text := result.record.Markdown.Summary
		r.emit(Event{Kind: "tool", Actor: actor, Text: text, EntryID: entry, CallID: ids[result.index]})
		close(result.stored)
	}
	workers.Wait()
	return records, batchErr
}
