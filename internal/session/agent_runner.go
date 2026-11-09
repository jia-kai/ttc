package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/prompts"
	"ttc/internal/render"
	"ttc/internal/tool"
)

// agentContext owns an actor's request projection and response accounting.
// Main reads the durable branch; assignments extend their isolated transcript.
// Neither implementation owns execution or the lifetime of background work.
type agentContext interface {
	admit(context.Context, *llm.Message) (agentRequest, error)
	append(...llm.Message)
	observe(int64, llm.Selection, *llm.Usage, bool)
}

type agentRequest struct {
	id int64
	llm.Request
}

// agentPolicy owns the response journal and the terminal admission decision.
// Staging precedes fallible persistence so assignment failures retain the last
// emitted answer. Completion runs only after a successful, settled tool cycle.
type agentPolicy interface {
	stage(llm.Message)
	committed(llm.Message, int64)
	complete(context.Context, llm.Selection, llm.Message) (bool, error)
}

// agentOutput describes presentation, not capabilities. Tool capabilities are
// supplied solely by the actor's registry. A zero text limit is unbounded.
type agentOutput struct {
	stdout, stderr io.Writer
	textLimit      int
	streamText     bool
	showReply      bool
	showUsage      bool
}

// AgentRunner executes the same model/tool/recovery cycle for main, coding and
// aside actors. It borrows actor state and resources; it never closes session or
// assignment work scopes. Execution returns after foreground tool settlement.
type AgentRunner struct {
	runtime *Runtime
	actor   string
	turn    string
	tools   *tool.Registry
	context agentContext
	policy  agentPolicy
	output  agentOutput
	// Assignments preserve empty assistant records; main omits empty output.
	saveEmpty bool
}

// agentResult contains settled model accounting, including failed requests.
// UsageKnown is false if any request omitted usable token accounting.
type agentResult struct {
	ModelTime    time.Duration
	OutputTokens int
	UsageKnown   bool
}

func (a *AgentRunner) run(ctx context.Context) (result agentResult, err error) {
	result.UsageKnown = true
	r := a.runtime
	priorAttempts := 0
	var recovery *llm.Message
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		request, err := a.context.admit(ctx, recovery)
		if err != nil {
			return result, err
		}
		reply := llm.Message{Type: llm.TypeAssistant}
		var text strings.Builder
		var usage *llm.Usage
		var responseID, serviceTier string
		callbackFailed := false
		started := time.Now()
		input := request.Request
		input.PriorAttempts = priorAttempts
		streamErr := r.stream(ctx, a.turn, a.actor, request.id, input, func(event llm.StreamEvent) (err error) {
			defer func() { callbackFailed = callbackFailed || err != nil }()
			if err := ctx.Err(); err != nil {
				return err
			}
			switch event.Kind {
			case "retry":
				return r.retryNotice(a.turn, a.actor, request.id, "coding", event.Retry)
			case "call_start":
				return r.toolAnnouncement(a.turn, a.actor, request.id, event.CallStart)
			case "call_progress":
				return r.toolProgress(a.actor, request.id, event.CallProgress)
			case "text":
				if a.output.textLimit > 0 && len(event.Text) > a.output.textLimit-text.Len() {
					return fmt.Errorf(prompts.ChildResponseTooLarge, a.output.textLimit)
				}
				text.WriteString(event.Text)
				if a.output.streamText {
					if a.output.stdout != nil {
						if _, err := io.WriteString(a.output.stdout, event.Text); err != nil {
							return err
						}
					}
					r.emit(Event{Kind: "delta", Actor: a.actor, Text: render.Clean(event.Text), RequestID: request.id})
				}
			case "call":
				if event.Call == nil {
					return errors.New(prompts.SessionProviderNilCall)
				}
				reply.Calls = append(reply.Calls, *event.Call)
			case "phase":
				reply.Phase = event.Phase
			case "state":
				return reply.AppendState(request.Selection, event.StateVersion, event.StateItem)
			case "completed":
				usage, responseID, serviceTier = event.Usage, event.ResponseID, event.ServiceTier
			}
			return nil
		})
		reply.Content = text.String()
		if streamErr != nil {
			// An interrupted callback can leave native state without its calls.
			// Keep canonical partial output, but never replay incomplete state.
			reply.State = nil
		}
		a.policy.stage(reply)
		r.recordUsage(usage)
		r.emit(Event{Kind: "tool_stream_end", PendingKey: pendingToolKey(request.id, ""), Text: streamState(streamErr)})
		elapsed := time.Since(started)
		result.ModelTime += elapsed
		if !usage.Usable() {
			result.UsageKnown = false
		} else {
			result.OutputTokens += usage.OutputTokens
		}
		status := "completed"
		if streamErr != nil {
			status = "failed"
		}
		attempt := history.RequestAttempt{DurationMS: elapsed.Milliseconds(), Usage: usage, ResponseID: responseID, ServiceTier: serviceTier, Status: status}
		if streamErr != nil {
			attempt.Error = streamErr.Error()
		}
		// Request completion appends chronology just like assistant/tool commits.
		// Exclude it while main handoff resolves and capacity-checks its tail.
		r.routeMu.RLock()
		err = r.Store.FinishRequest(request.id, history.RequestCompletion{Status: status, Attempts: []history.RequestAttempt{attempt}})
		r.routeMu.RUnlock()
		if err != nil {
			return result, err
		}
		a.context.observe(request.id, request.Selection, usage, streamErr == nil)
		if a.output.showUsage {
			r.emit(Event{Kind: "usage"})
		}
		var entry int64
		var ids []string
		if a.saveEmpty || reply.Content != "" || len(reply.Calls) != 0 || reply.State != nil {
			// Routing protects short commits, never streams or blocking tools.
			r.routeMu.RLock()
			entry, ids, err = r.Store.Assistant(r.Current(), a.turn, a.actor, request.id, reply)
			r.routeMu.RUnlock()
			if err != nil {
				return result, err
			}
			if a.output.showReply {
				r.emit(Event{Kind: "assistant", Actor: a.actor, Text: reply.Content, EntryID: entry, RequestID: request.id})
			}
		}
		a.context.append(reply)
		a.policy.committed(reply, entry)
		records, err := r.runToolBatch(ctx, a.turn, a.actor, a.tools, reply.Calls, ids, streamErr)
		if err != nil {
			return result, err
		}
		for i, record := range records {
			if a.output.stderr != nil {
				if _, err := fmt.Fprintln(a.output.stderr, record.Markdown.Summary); err != nil {
					return result, err
				}
			}
			a.context.append(llm.Message{Type: llm.TypeToolResult, CallID: reply.Calls[i].ID, Content: string(record.Result), Files: record.Files})
		}
		if a.output.showUsage {
			r.emit(Event{Kind: "usage"})
		}
		if streamErr != nil {
			var partial *llm.PartialError
			if callbackFailed || !errors.As(streamErr, &partial) {
				return result, streamErr
			}
			message, err := r.recoverPartial(ctx, a.turn, a.actor, request.id, priorAttempts, partial)
			if err != nil {
				return result, err
			}
			a.context.append(message)
			recovery = &message
			priorAttempts = partial.Retry.Attempt - 1
			continue
		}
		priorAttempts, recovery = 0, nil
		done, err := a.policy.complete(ctx, request.Selection, reply)
		if err != nil || done {
			return result, err
		}
	}
}
