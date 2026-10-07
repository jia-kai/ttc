package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"ttc/internal/prompts"
	"unicode"
	"unicode/utf8"

	"ttc/internal/history"
	"ttc/internal/jobs"
	"ttc/internal/llm"
	"ttc/internal/tool"
)

// maxChildSlots bounds retained coding children (including idle children) plus
// running /btw asides in one runtime.
const maxChildSlots = 16

func (r *Runtime) addSubagentTool() {
	type args struct {
		Prompt     string  `json:"prompt"`
		ChildID    string  `json:"child_id,omitempty"`
		Label      string  `json:"label,omitempty"`
		Background bool    `json:"background,omitempty"`
		Persistent *bool   `json:"persistent"`
		Variant    *string `json:"variant,omitempty"`
	}
	tool.Register(r.Tools, "subagent", fmt.Sprintf(prompts.ToolDescription("subagent"), maxChildSlots), map[string]any{"prompt": tool.Property("string"), "child_id": tool.Property("string"), "label": tool.Property("string"), "background": tool.Property("boolean"), "persistent": tool.Property("boolean"), "variant": tool.Property("string")}, []string{"prompt", "persistent"}, func(a args) error {
		if err := tool.Required("prompt", a.Prompt); err != nil {
			return err
		}
		if a.Persistent == nil {
			return tool.Fail("invalid_arguments", prompts.ChildExplicitPersistence)
		}
		if a.Variant != nil && strings.TrimSpace(*a.Variant) == "" {
			return tool.Fail("invalid_arguments", prompts.ChildNonemptyVariant)
		}
		if a.ChildID != "" {
			if a.Label != "" {
				return tool.Fail("invalid_arguments", prompts.ChildFollowupLabel)
			}
			return nil
		}
		if words := len(strings.Fields(a.Label)); words < 1 || words > 4 || utf8.RuneCountInString(a.Label) > 64 || strings.IndexFunc(a.Label, func(r rune) bool { return unicode.IsControl(r) || r == '\u2028' || r == '\u2029' }) >= 0 {
			return tool.Fail("invalid_arguments", prompts.ChildValidLabel)
		}
		return nil
	}, func(ctx context.Context, x tool.Execution, a args) (any, error) {
		if x.Actor != "main" {
			return nil, tool.Fail("ownership", prompts.ChildCannotAssign)
		}
		if err := r.checkContext(); err != nil {
			return nil, err
		}
		var modelJSON string
		if err := r.Store.DB.QueryRow("SELECT q.model_json FROM tool_calls c JOIN model_requests q ON q.id=c.request_id WHERE c.id=?", x.CallID).Scan(&modelJSON); err != nil {
			return nil, err
		}
		var selection llm.Selection
		if err := json.Unmarshal([]byte(modelJSON), &selection); err != nil {
			return nil, err
		}
		r.childStartMu.Lock()
		child := r.children[a.ChildID]
		if a.ChildID != "" {
			if child == nil {
				r.childStartMu.Unlock()
				return nil, tool.Fail("not_found", prompts.ChildUnknownFollowup)
			}
			if child.state != "idle" || child.closing {
				r.childStartMu.Unlock()
				return nil, tool.Fail("child_busy", prompts.ChildFollowupBusy)
			}
			selection = child.selection
		}
		if a.Variant != nil {
			if !slices.Contains(selection.Model.Variants, *a.Variant) {
				r.childStartMu.Unlock()
				return nil, tool.Fail("invalid_arguments", fmt.Sprintf(prompts.ChildUnsupportedVariant, *a.Variant, selection.Model.ID, strings.Join(selection.Model.Variants, ", ")))
			}
			selection.Variant = *a.Variant
		}
		if child == nil {
			asideCount := 0
			for _, job := range r.Jobs.Live() {
				if job.Kind == "btw" {
					asideCount++
				}
			}
			if len(r.children)+asideCount >= maxChildSlots {
				r.childStartMu.Unlock()
				return nil, tool.Fail("capacity", fmt.Sprintf(prompts.ChildCapacity, maxChildSlots))
			}
			child = &codingChild{id: "main/" + history.NewID("child"), label: strings.Join(strings.Fields(a.Label), " "), tools: r.Tools.Filter(func(name string) bool { return name != "subagent" && name != "question" })}
			r.children[child.id] = child
		}
		child.selection = selection
		child.state = "running"
		child.job = ""
		turn, err := r.Store.BeginChildTurn(r.Current(), child.id, child.selection)
		if err != nil {
			delete(r.children, child.id)
			r.childStartMu.Unlock()
			return nil, err
		}
		child.turn = turn
		assignment := &childAssignment{
			turn: turn, persistent: *a.Persistent, background: a.Background,
			answer: child.lastAnswer, truncated: child.lastTruncated, result: child.lastResult,
			inheritedAnswer: child.lastAnswer != "",
		}
		task := childTask{assignment: assignment, actor: child.id, turn: turn, prompt: a.Prompt, selection: child.selection, prefix: child.messages, cursor: child.cursor, tools: child.tools, child: child}
		ready := make(chan struct{})
		r.childStartMu.Unlock()
		id, err := r.Jobs.StartTask(child.id, "subagent", child.label, a.Background, false, func(childCtx context.Context, stdout, stderr io.Writer) (runErr error) {
			<-ready
			defer func() {
				task.cancelled = childCtx.Err() != nil
				if finishErr := r.finishChild(child, task, runErr); finishErr != nil {
					runErr = errors.Join(runErr, finishErr)
				}
			}()
			return r.runChild(childCtx, task, stdout, stderr)
		})
		r.childStartMu.Lock()
		child.job = id
		assignment.job = id
		if err == nil {
			err = r.publishChild(tool.ChildView{ID: child.id, Label: child.label, State: child.state, TurnID: turn, JobID: id})
		}
		r.childStartMu.Unlock()
		close(ready)
		if err != nil {
			if id != "" {
				_, _ = r.Jobs.Stop("main", id)
			} else {
				err = errors.Join(err, r.finishChild(child, task, err))
				result := r.childResult(child, assignment, jobs.Snapshot{Kind: "subagent", Owner: child.id, Label: child.label, Status: "failed"})
				if !a.Background {
					result["error"] = err.Error()
				}
				return result, nil // Failed completion/launch metadata is the tool result; storage failures also abort the runtime.
			}
			return nil, err
		}
		if a.Background {
			v, e := r.Jobs.View("main", id)
			return r.childResult(child, assignment, v), e
		}
		v, err := r.Jobs.Wait(ctx, "main", id, func(v jobs.Snapshot) {
			if x.Update != nil {
				x.Update(r.childResult(child, assignment, v))
			}
		})
		return r.childResult(child, assignment, v), err
	})
}

// childTask freezes one child request's context, capabilities and model selection.
type childTask struct {
	actor, turn, prompt string
	selection           llm.Selection
	prefix              []llm.Message
	tools               *tool.Registry
	aside               bool
	child               *codingChild
	assignment          *childAssignment
	cursor              contextCursor
	cancelled           bool
}

func (r *Runtime) runChild(ctx context.Context, task childTask, stdout, stderr io.Writer) error {
	actor, turn, prompt, selection := task.actor, task.turn, task.prompt, task.selection
	defer r.clearActorImage(actor, "")
	messages := append([]llm.Message(nil), task.prefix...)
	r.routeMu.RLock()
	if task.aside {
		instruction := llm.Message{Role: "developer", Content: btwInstruction, Runtime: true}
		id, err := r.Store.Append(r.Current(), turn, actor, "message", "developer", false, instruction)
		if err != nil {
			r.routeMu.RUnlock()
			return err
		}
		r.emit(Event{Kind: "message_placeholder", Actor: actor, Text: "Aside instructions · inspect", EntryID: id})
		messages = append(messages, instruction)
	}
	question := llm.Message{Role: "user", Content: prompt, InputSource: "task"}
	if task.aside {
		question.InputSource = "btw"
	}
	entry, err := r.Store.Append(r.Current(), turn, actor, "message", "user", false, question)
	if err == nil {
		var committed history.Entry
		committed, err = r.Store.Entry(entry)
		question.InputTimeMS = committed.CreatedMS
	}
	r.routeMu.RUnlock()
	if err != nil {
		return err
	}
	messages = append(messages, question)
	r.emit(Event{Kind: "message", Actor: actor, Text: prompt, EntryID: entry})
	defs := task.tools.Definitions()
	cursor := task.cursor
	priorAttempts := 0
	var recovery *llm.Message
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		system := childSystemTemplate
		if task.aside {
			system = systemTemplate
		}
		r.routeMu.RLock()
		admitted, nextContext, err := r.admitChild(ctx, task, messages, cursor)
		if errors.Is(err, errNeedsCompaction) {
			r.routeMu.RUnlock()
			messages, cursor, err = r.compactChild(ctx, task, messages, cursor, recovery)
			if err != nil {
				return err
			}
			continue
		}
		if err != nil {
			r.routeMu.RUnlock()
			return err
		}
		request := admitted.RequestID
		cursor = nextContext
		messages = admitted.Messages
		if admitted.ContextEntry != 0 {
			r.emit(Event{Kind: "runtime_context", Actor: actor, Text: "Runtime context · inspect", EntryID: admitted.ContextEntry, SessionID: r.Current()})
		}
		entry, err = r.Store.RecordSystemPrompt(r.Current(), turn, actor, request, system)
		r.routeMu.RUnlock()
		if err != nil {
			return err
		}
		r.emit(Event{Kind: "system_prompt", Actor: actor, Text: "System prompt · inspect", EntryID: entry})
		reply := llm.Message{Role: "assistant"}
		var text strings.Builder
		started := time.Now()
		var usage *llm.Usage
		var responseID, serviceTier string
		callbackFailed := false
		streamErr := r.Provider.Stream(ctx, llm.Request{ConversationID: actor, Selection: selection, System: system, Messages: messages, Tools: defs, OutputTokens: selection.Model.Budget.OutputAllowance, PriorAttempts: priorAttempts}, func(ev llm.StreamEvent) (err error) {
			defer func() { callbackFailed = callbackFailed || err != nil }()
			if err := ctx.Err(); err != nil {
				return err
			}
			switch ev.Kind {
			case "retry":
				return r.retryNotice(turn, actor, request, "coding", ev.Retry)
			case "call_start":
				return r.toolAnnouncement(turn, actor, request, ev.CallStart)
			case "call_progress":
				return r.toolProgress(actor, request, ev.CallProgress)
			case "completed":
				usage, responseID, serviceTier = ev.Usage, ev.ResponseID, ev.ServiceTier
			case "text":
				limit := 32 << 20
				if task.aside {
					limit = 64 << 10
				}
				if text.Len()+len(ev.Text) > limit {
					return fmt.Errorf(prompts.ChildResponseTooLarge, limit)
				}
				text.WriteString(ev.Text)
				if task.aside {
					return nil
				}
				_, err := io.WriteString(stdout, ev.Text)
				return err
			case "call":
				if ev.Call == nil {
					return errors.New(prompts.SessionProviderNilCall)
				}
				reply.Calls = append(reply.Calls, *ev.Call)
			case "phase":
				reply.Phase = ev.Phase
			case "state":
				return reply.AppendState(selection, ev.StateVersion, ev.StateItem)
			}
			return nil
		})
		reply.Content = text.String()
		if task.child != nil && strings.TrimSpace(reply.Content) != "" {
			// Capture before storage/tool settlement: local failures must still
			// hand back the last assistant text, including an interrupted stream.
			r.childStartMu.Lock()
			task.assignment.answer, task.assignment.truncated = childAnswer(reply.Content)
			task.assignment.result = 0
			task.assignment.inheritedAnswer = false
			r.childStartMu.Unlock()
		}
		r.recordUsage(usage)
		if streamErr != nil {
			reply.State = nil // Interrupted completion callbacks may leave an incomplete replay payload.
		}
		if task.child != nil {
			r.childStartMu.Lock()
			task.assignment.pending = &reply
			r.childStartMu.Unlock()
		}
		r.emit(Event{Kind: "tool_stream_end", PendingKey: pendingToolKey(request, ""), Text: streamState(streamErr)})
		status := "completed"
		if streamErr != nil {
			status = "failed"
		}
		attempt := map[string]any{"duration_ms": time.Since(started).Milliseconds(), "usage": usage, "response_id": responseID, "service_tier": serviceTier, "status": status}
		if streamErr != nil {
			attempt["error"] = streamErr.Error()
		}
		if err := r.Store.FinishRequest(request, status, []any{attempt}); err != nil {
			return err
		}
		r.routeMu.RLock()
		entry, ids, err := r.Store.Assistant(r.Current(), turn, actor, request, reply)
		r.routeMu.RUnlock()
		if err != nil {
			return err
		}
		if task.aside {
			r.emit(Event{Kind: "message_placeholder", Actor: actor, Text: "Reply · inspect", EntryID: entry})
		} else {
			r.emit(Event{Kind: "assistant", Text: reply.Content, EntryID: entry, RequestID: request, Actor: actor})
		}
		messages = append(messages, reply)
		if task.child != nil {
			r.childStartMu.Lock()
			if strings.TrimSpace(reply.Content) != "" {
				task.assignment.result = entry
			}
			task.assignment.pending = nil
			r.childStartMu.Unlock()
		}
		records, err := r.runToolBatch(ctx, turn, actor, task.tools, reply.Calls, ids, streamErr)
		if err != nil {
			return err
		}
		for i, record := range records {
			if _, err := fmt.Fprintln(stderr, record.Markdown.Summary); err != nil {
				return err
			}
			messages = append(messages, llm.Message{Role: "tool", CallID: reply.Calls[i].ID, Content: string(record.Result), Files: record.Files})
		}
		if streamErr != nil {
			var partial *llm.PartialError
			if callbackFailed || !errors.As(streamErr, &partial) {
				return streamErr
			}
			message, err := r.recoverPartial(ctx, turn, actor, request, priorAttempts, partial)
			if err != nil {
				return err
			}
			messages = append(messages, message)
			recovery = &message
			priorAttempts = partial.Retry.Attempt - 1
			continue
		}
		priorAttempts, recovery = 0, nil
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
			r.emit(Event{Kind: "message", Actor: actor, Text: notification.Content, EntryID: id})
		} else if len(reply.Calls) == 0 {
			if task.child != nil {
				if strings.TrimSpace(reply.Content) == "" {
					return errors.New(prompts.ChildCompletedWithoutText)
				}
				r.childStartMu.Lock()
				task.child.messages = messages
				task.child.cursor = cursor
				r.childStartMu.Unlock()
			}
			return nil
		}
	}
}
