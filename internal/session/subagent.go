package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"ttc/internal/prompts"
	"unicode"
	"unicode/utf8"

	contextbuild "ttc/internal/context"
	"ttc/internal/history"
	"ttc/internal/jobs"
	"ttc/internal/llm"
	"ttc/internal/tool"
)

// maxChildSlots bounds retained coding children (including idle children) plus
// running /btw asides in one runtime.
const maxChildSlots = 16

func (r *Runtime) addSubagentTool() {
	r.addChildSteerTool()
	type args struct {
		Prompt        string   `json:"prompt"`
		ChildID       string   `json:"child_id,omitempty"`
		Label         string   `json:"label,omitempty"`
		Background    bool     `json:"background,omitempty"`
		Persistent    *bool    `json:"persistent"`
		Easy          *bool    `json:"easy,omitempty"`
		JobReadGrants []string `json:"job_read_grants,omitempty"`
	}
	tool.Register(r.Tools, "subagent", fmt.Sprintf(prompts.ToolDescription("subagent"), maxChildSlots), map[string]any{"prompt": tool.Property("string"), "child_id": tool.Property("string"), "label": tool.Property("string"), "background": tool.Property("boolean"), "persistent": tool.Property("boolean"), "easy": tool.Property("boolean"), "job_read_grants": map[string]any{"type": "array", "items": tool.Property("string"), "maxItems": maxJobReadGrants}}, []string{"prompt", "persistent"}, func(a args) error {
		if err := validateReadGrantCount(a.JobReadGrants); err != nil {
			return err
		}
		if err := tool.Required("prompt", a.Prompt); err != nil {
			return err
		}
		if a.Persistent == nil {
			return tool.Fail("invalid_arguments", prompts.ChildExplicitPersistence)
		}
		if a.ChildID != "" {
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
		if err := r.checkContext(false); err != nil {
			return nil, err
		}
		a.JobReadGrants = distinctReadGrants(a.JobReadGrants)
		if err := r.Jobs.ValidateReadGrants(x.Actor, a.JobReadGrants); err != nil {
			return nil, tool.Fail("not_found", err.Error())
		}
		selection, easySelection, err := r.Store.ToolModelSelections(ctx, x.CallID)
		if err != nil {
			return nil, err
		}
		r.coordinator.childMu.Lock()
		child := r.coordinator.children[a.ChildID]
		if a.ChildID != "" {
			if child == nil {
				r.coordinator.childMu.Unlock()
				return nil, tool.Fail("not_found", prompts.ChildUnknownFollowup)
			}
			if a.Label != "" && strings.Join(strings.Fields(a.Label), " ") != child.label {
				r.coordinator.childMu.Unlock()
				return nil, tool.Fail("invalid_arguments", prompts.ChildFollowupLabel)
			}
			if child.state != "idle" || child.closing {
				r.coordinator.childMu.Unlock()
				return nil, tool.Fail("child_busy", prompts.ChildFollowupBusy)
			}
			selection = child.selection
		}
		easy := a.Easy != nil && *a.Easy
		if child == nil && easy {
			if easySelection != nil {
				selection = *easySelection
			}
		}
		if child == nil {
			asideCount := 0
			for _, job := range r.Jobs.Live() {
				if job.Kind == "btw" {
					asideCount++
				}
			}
			if len(r.coordinator.children)+asideCount >= maxChildSlots {
				r.coordinator.childMu.Unlock()
				return nil, tool.Fail("capacity", fmt.Sprintf(prompts.ChildCapacity, maxChildSlots))
			}
			child = &codingChild{id: "main/" + history.NewID("child"), label: strings.Join(strings.Fields(a.Label), " "), easy: easy, estimator: &contextbuild.Estimator{}, tools: r.Tools.Filter(func(name string) bool { return name != "subagent" && name != "child_steer" && name != "question" })}
			r.coordinator.children[child.id] = child
		}
		turn, err := r.Store.BeginChildTurn(r.Current(), child.id, selection)
		if err != nil {
			if a.ChildID == "" {
				delete(r.coordinator.children, child.id)
			}
			r.coordinator.childMu.Unlock()
			return nil, err
		}
		// Preserve the idle context and selection if durable admission fails.
		if child.selection.Provider != selection.Provider || child.selection.Model.ID != selection.Model.ID || child.selection.Model.RequestID() != selection.Model.RequestID() || child.selection.Model.ServiceTier != selection.Model.ServiceTier || child.selection.Variant != selection.Variant {
			child.estimator.Reset()
		}
		child.selection = selection
		child.state = "running"
		child.job = ""
		child.turn = turn
		assignment := &childAssignment{
			executionID:     r.coordinator.executions.begin(child.id, "subagent", "", r.Generation()).ID,
			inspection:      newChildPromptInspection(a.Prompt),
			accepting:       true,
			queueTokenLimit: min(maxChildMessageQueueTokens, selection.Model.Budget.NextTurnInputReserve),
			turn:            turn, persistent: *a.Persistent, background: a.Background,
			answer: child.lastAnswer, truncated: child.lastTruncated, result: child.lastResult,
			inheritedAnswer:   child.lastAnswer != "",
			initialReadGrants: a.JobReadGrants,
		}
		child.assignment = assignment
		r.Jobs.BeginOwned(child.id)
		r.orderMu.Lock()
		r.actorAssignments[child.id] = assignment
		r.orderMu.Unlock()
		task := childTask{assignment: assignment, actor: child.id, turn: turn, prompt: a.Prompt, selection: child.selection, prefix: child.messages, cursor: child.cursor, tools: child.tools, child: child, estimator: child.estimator}
		ready := make(chan struct{})
		r.coordinator.childMu.Unlock()
		id, err := r.Jobs.StartTask(child.id, jobs.TaskOptions{Kind: "subagent", Label: child.label, Background: a.Background, Easy: child.easy, Inspector: assignment.inspection.inspect}, func(childCtx context.Context, stdout, stderr io.Writer) (result jobs.TaskResult, runErr error) {
			<-ready
			defer func() {
				task.cancelled = childCtx.Err() != nil
				if finishErr := r.finishChild(child, task, runErr); finishErr != nil {
					runErr = errors.Join(runErr, finishErr)
				}
			}()
			return jobs.TaskResult{}, r.runChild(childCtx, task, stdout, stderr)
		})
		r.coordinator.childMu.Lock()
		child.job = id
		assignment.job = id
		if err == nil {
			err = r.publishChild(child.view())
		}
		r.coordinator.childMu.Unlock()
		close(ready)
		if err != nil {
			if id != "" {
				_, _ = r.Jobs.Stop("main", id)
			} else {
				err = errors.Join(err, r.finishChild(child, task, err))
				result := r.childResult(child, assignment, jobs.Snapshot{Kind: "subagent", Owner: child.id, Label: child.label, Status: "failed"})
				if !a.Background {
					result.Error = err.Error()
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
	}, func(a *args) []tool.InputWarning {
		if a.ChildID == "" {
			return nil
		}
		var warnings []tool.InputWarning
		if a.Easy != nil {
			r.coordinator.childMu.Lock()
			child := r.coordinator.children[a.ChildID]
			if child != nil {
				warnings = append(warnings, tool.InputWarning{Code: "input_ignored", Parameter: "easy", Requested: *a.Easy, Effective: child.easy, Message: prompts.ChildFollowupEasy})
			}
			r.coordinator.childMu.Unlock()
		}
		normalized := strings.Join(strings.Fields(a.Label), " ")
		if normalized != a.Label {
			warnings = append(warnings, tool.AdjustedInput("label", a.Label, normalized)...)
			a.Label = normalized
		}
		return warnings
	})
}

// childTask freezes one child request's context, capabilities and model selection.
type childTask struct {
	actor, turn, prompt string
	selection           llm.Selection
	prefix              []llm.Message
	tools               *tool.Registry
	estimator           *contextbuild.Estimator // Optional input calibration; independently owned, retained only by this child.
	aside               bool
	child               *codingChild
	assignment          *childAssignment
	cursor              contextCursor
	cancelled           bool
}

func (r *Runtime) runChild(ctx context.Context, task childTask, stdout, stderr io.Writer) error {
	if task.estimator == nil {
		task.estimator = &contextbuild.Estimator{}
	}
	actor, turn, prompt := task.actor, task.turn, task.prompt
	defer r.clearActorImage(actor, "")
	messages := append([]llm.Message(nil), task.prefix...)
	var grantNotice *llm.Message
	if task.assignment != nil && len(task.assignment.initialReadGrants) != 0 {
		notice := llm.NewJobReadGrantNotice(task.assignment.initialReadGrants)
		grantNotice = &notice
	}
	var publications *publicationReservation
	if task.aside {
		publications = r.reservePublications(1)
		defer publications.release()
	}
	r.routeMu.RLock()
	if task.aside {
		instruction := llm.Message{Type: llm.TypeRuntimeInstruction, Content: btwInstruction}
		id, err := r.Store.Append(r.Current(), turn, actor, "message", "developer", false, instruction)
		if err != nil {
			r.routeMu.RUnlock()
			return err
		}
		r.emitReserved(publications, Event{Kind: "message", Actor: actor, MessageType: instruction.Type, EntryID: id})
		messages = append(messages, instruction)
	}
	question := llm.Message{Type: llm.TypeUserInput, Content: prompt, InputSource: "task"}
	if task.aside {
		question.InputSource = "btw"
	}
	entry, err := r.Store.Append(r.Current(), turn, actor, "message", "user", false, question)
	if err == nil {
		var committed history.Entry
		committed, err = r.Store.Entry(entry)
		question.InputTimeMS = committed.CreatedMS
	}
	var grantEntry int64
	if err == nil && grantNotice != nil {
		// Assignment metadata has no source event to acknowledge. Persist it
		// beside the task, once, rather than queueing it for request retries.
		grantEntry, err = r.Store.Append(r.Current(), turn, actor, "message", "user", false, *grantNotice)
	}
	r.routeMu.RUnlock()
	if publications != nil {
		publications.release()
	}
	if err != nil {
		return err
	}
	messages = append(messages, question)
	r.emit(Event{Kind: "message", Actor: actor, Text: prompt, EntryID: entry, MessageType: question.Type})
	if grantNotice != nil {
		messages = append(messages, *grantNotice)
		r.emit(Event{Kind: "message", Actor: actor, Text: grantNotice.Content, EntryID: grantEntry, MessageType: grantNotice.Type})
	}
	actorContext := &assignmentAgentContext{
		runtime: r, task: task, messages: messages, cursor: task.cursor, system: childSystemTemplate,
	}
	var policy agentPolicy = &assignmentAgentPolicy{runtime: r, context: actorContext}
	output := agentOutput{stdout: stdout, stderr: stderr, textLimit: 32 << 20, streamText: true, showReply: true}
	if task.aside {
		actorContext.system = asideSystemTemplate
		policy = &asideAgentPolicy{stdout: stdout}
		output.textLimit, output.streamText, output.showReply = 64<<10, false, false
	}
	runner := AgentRunner{runtime: r, actor: actor, turn: turn, tools: task.tools, context: actorContext, policy: policy, output: output, saveEmpty: true}
	_, err = runner.run(ctx)
	return err
}
