package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	contextbuild "scicode/internal/context"
	"scicode/internal/history"
	"scicode/internal/jobs"
	"scicode/internal/provider"
	"scicode/internal/tool"
)

func (r *Runtime) addSubagentTool() {
	type args struct {
		Prompt     string `json:"prompt"`
		Label      string `json:"label"`
		Background bool   `json:"background,omitempty"`
		Wake       *bool  `json:"wake_on_exit,omitempty"`
	}
	tool.Register(r.Tools, "subagent", "Run an isolated child coding task with shared file tools and bounded output. label is a concise single-line UI title, 1–64 characters, without control characters. At most four live children; children cannot spawn children. Use job_read to inspect its stdout reply or stderr tool feedback.", map[string]any{"prompt": tool.Property("string"), "label": map[string]any{"type": "string", "minLength": 1, "maxLength": 64, "description": "Concise single-line task title, no control characters"}, "background": tool.Property("boolean"), "wake_on_exit": tool.Property("boolean")}, []string{"prompt", "label"}, func(a args) error {
		if err := tool.Required("prompt", a.Prompt); err != nil {
			return err
		}
		if strings.TrimSpace(a.Label) == "" || utf8.RuneCountInString(a.Label) > 64 || strings.IndexFunc(a.Label, unicode.IsControl) >= 0 {
			return tool.Fail("invalid_arguments", "label must be a nonblank single-line title of at most 64 characters without controls")
		}
		return nil
	}, func(ctx context.Context, x tool.Execution, a args) (any, error) {
		if x.Actor != "main" {
			return nil, tool.Fail("capacity", "children cannot spawn children")
		}
		var turn, modelJSON string
		if err := r.Store.DB.QueryRow("SELECT coalesce(q.turn_id,''),q.model_json FROM tool_calls c JOIN model_requests q ON q.id=c.request_id WHERE c.id=?", x.CallID).Scan(&turn, &modelJSON); err != nil {
			return nil, err
		}
		var selection provider.Selection
		if err := json.Unmarshal([]byte(modelJSON), &selection); err != nil {
			return nil, err
		}
		actor := "main/" + history.NewID("child")
		id, err := func() (string, error) {
			r.childStartMu.Lock()
			defer r.childStartMu.Unlock()
			count := 0
			for _, j := range r.Jobs.List("main", false) {
				if j.Kind == "subagent" || j.Kind == "btw" {
					count++
				}
			}
			if count >= 4 {
				return "", tool.Fail("capacity", "four child tasks are already running")
			}
			return r.Jobs.StartTask(actor, "subagent", a.Label, a.Background, a.Background && (a.Wake == nil || *a.Wake), func(child context.Context, stdout, stderr io.Writer) error {
				return r.runChild(child, childTask{actor: actor, turn: turn, prompt: a.Prompt, selection: selection, tools: r.Tools.Filter(func(name string) bool { return name != "subagent" })}, stdout, stderr)
			})
		}()
		if err != nil {
			return nil, err
		}
		if a.Background {
			return r.Jobs.View("main", id)
		}
		return r.Jobs.Wait(ctx, "main", id, func(v jobs.Snapshot) {
			if x.Update != nil {
				x.Update(v)
			}
		})
	})
}

// childTask freezes one child request's context, capabilities and model selection.
type childTask struct {
	actor, turn, prompt string
	selection           provider.Selection
	prefix              []provider.Message
	tools               *tool.Registry
	aside               bool
}

func (r *Runtime) runChild(ctx context.Context, task childTask, stdout, stderr io.Writer) error {
	actor, turn, prompt, selection := task.actor, task.turn, task.prompt, task.selection
	defer r.clearActorImage(actor, "")
	messages := append([]provider.Message(nil), task.prefix...)
	r.routeMu.RLock()
	if task.aside {
		instruction := provider.Message{Role: "developer", Content: btwInstruction, Runtime: true}
		id, err := r.Store.Append(r.Current(), turn, actor, "message", "developer", false, instruction)
		if err != nil {
			r.routeMu.RUnlock()
			return err
		}
		r.emit(Event{Kind: "message_placeholder", Text: actor + " · aside instructions · inspect", EntryID: id})
		messages = append(messages, instruction)
	}
	question := provider.Message{Role: "user", Content: prompt}
	messages = append(messages, question)
	entry, err := r.Store.Append(r.Current(), turn, actor, "message", "user", false, question)
	r.routeMu.RUnlock()
	if err != nil {
		return err
	}
	r.emit(Event{Kind: "message", Text: actor + " · " + prompt, EntryID: entry})
	defs := task.tools.Definitions()
	var cursor contextCursor
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		system := childSystemTemplate
		if task.aside {
			system = systemTemplate
		}
		contextMessage, nextContext, err := r.runtimeContext(ctx, actor, selection, cursor)
		if err != nil {
			return err
		}
		requestMessages := append(append([]provider.Message(nil), messages...), contextMessage)
		if !contextbuild.Fits(selection, system, defs, requestMessages, false) {
			return tool.Fail("context_overflow", "child task exceeds its model context")
		}
		r.routeMu.RLock()
		request, err := r.Store.StartRequest(r.Current(), turn, actor, "coding", selection)
		if err != nil {
			r.routeMu.RUnlock()
			return err
		}
		contextMessage.RequestID = request
		entry, err := r.Store.Append(r.Current(), turn, actor, "message", "developer", false, contextMessage)
		if err != nil {
			r.routeMu.RUnlock()
			return err
		}
		cursor = nextContext
		messages = append(messages, contextMessage)
		r.emit(Event{Kind: "runtime_context", Text: actor + " · " + contextLabel(contextMessage), EntryID: entry, SessionID: r.Current()})
		entry, err = r.Store.RecordSystemPrompt(r.Current(), turn, actor, request, system)
		r.routeMu.RUnlock()
		if err != nil {
			return err
		}
		r.emit(Event{Kind: "system_prompt", Text: actor + " · System prompt · inspect", EntryID: entry})
		reply := provider.Message{Role: "assistant"}
		started := time.Now()
		var usage *provider.Usage
		var responseID, serviceTier string
		streamErr := r.Provider.Stream(ctx, provider.Request{ConversationID: actor, Selection: selection, System: system, Messages: messages, Tools: defs, OutputTokens: selection.Model.Budget.OutputAllowance}, func(ev provider.StreamEvent) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			switch ev.Kind {
			case "retry":
				return r.retryNotice(turn, actor, request, ev.Retry)
			case "call_start":
				return r.toolAnnouncement(turn, actor, request, ev.CallStart)
			case "completed":
				usage, responseID, serviceTier = ev.Usage, ev.ResponseID, ev.ServiceTier
			case "text":
				limit := 32 << 20
				if task.aside {
					limit = 64 << 10
				}
				if len(reply.Content)+len(ev.Text) > limit {
					return fmt.Errorf("child response exceeds %d bytes", limit)
				}
				reply.Content += ev.Text
				if task.aside {
					return nil
				}
				_, err := io.WriteString(stdout, ev.Text)
				return err
			case "call":
				if ev.Call == nil {
					return errors.New("provider emitted nil call")
				}
				reply.Calls = append(reply.Calls, *ev.Call)
			case "phase":
				reply.Phase = ev.Phase
			case "state":
				return reply.AppendState(selection, ev.StateVersion, ev.StateItem)
			}
			return nil
		})
		r.recordUsage(usage)
		if streamErr != nil {
			reply.State = nil // Interrupted completion callbacks may leave an incomplete replay payload.
		}
		r.emit(Event{Kind: "tool_stream_end", PendingKey: pendingToolKey(request, ""), Text: streamState(streamErr)})
		status := "completed"
		if streamErr != nil {
			status = "failed"
		}
		if err := r.Store.FinishRequest(request, status, []any{map[string]any{"duration_ms": time.Since(started).Milliseconds(), "usage": usage, "response_id": responseID, "service_tier": serviceTier, "status": status}}); err != nil {
			return err
		}
		r.routeMu.RLock()
		entry, ids, err := r.Store.Assistant(r.Current(), turn, actor, request, reply)
		r.routeMu.RUnlock()
		if err != nil {
			return err
		}
		if task.aside {
			r.emit(Event{Kind: "message_placeholder", Text: actor + " · reply · inspect", EntryID: entry})
		} else {
			r.emit(Event{Kind: "assistant", Text: reply.Content, EntryID: entry, RequestID: request, Actor: actor})
		}
		messages = append(messages, reply)
		records, err := r.runToolBatch(ctx, turn, actor, task.tools, reply.Calls, ids, streamErr)
		if err != nil {
			return err
		}
		for i, record := range records {
			if _, err := fmt.Fprintln(stderr, record.Markdown.Summary); err != nil {
				return err
			}
			messages = append(messages, provider.Message{Role: "tool", CallID: reply.Calls[i].ID, Content: string(record.Result)})
		}
		if streamErr != nil {
			return streamErr
		}
		if task.aside && len(reply.Calls) == 0 {
			_, err := io.WriteString(stdout, reply.Content)
			return err
		}
		notification, err := r.childImageReply(ctx, actor, len(reply.Calls) == 0)
		if err != nil {
			return err
		}
		if notification != nil {
			r.routeMu.RLock()
			id, err := r.Store.Append(r.Current(), turn, actor, "message", "user", false, *notification)
			r.routeMu.RUnlock()
			if err != nil {
				return err
			}
			messages = append(messages, *notification)
			r.emit(Event{Kind: "message", Text: actor + " · " + notification.Content, EntryID: id})
		} else if len(reply.Calls) == 0 {
			return nil
		}
	}
}
