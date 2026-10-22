package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"
	"ttc/internal/prompts"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
)

// compactionTimeout sets the cooperative deadline for the entire operation.
// Earlier owning-context deadlines apply; synchronous storage may delay return.
const compactionTimeout = 10 * time.Minute

// compactionCapacityError leaves the admitted conversation unchanged. A smaller
// focus/input or a different model can make a subsequent attempt fit.
type compactionCapacityError struct{ message string }

func (e *compactionCapacityError) Error() string { return e.message }

// Recoverable failures leave the existing context usable. Unknown errors,
// malformed summaries and broken persistence conservatively invalidate it.
func recoverableCompaction(err error) bool {
	if _, ok := err.(*compactionCapacityError); ok {
		return true
	}
	if err == contextbuild.ErrNoCompactableHistory {
		return true
	}
	if _, ok := err.(*llm.TransientError); ok {
		return true
	}
	if _, ok := err.(net.Error); ok {
		return true
	}
	// A transient provider failure joined with a broken history write is fatal.
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if !recoverableCompaction(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return recoverableCompaction(wrapped.Unwrap())
	}
	var network net.Error
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.As(err, &network) || errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

func (r *Runtime) compactionFailure(session string, err error) error {
	if err == nil || recoverableCompaction(err) {
		return err
	}
	r.mu.Lock()
	r.fatalCompaction = err.Error()
	r.mu.Unlock()
	invalid := r.Workspace.Admit(context.Background(), func() error { return r.Store.InvalidateContext(session, err.Error()) })
	r.Jobs.Close()
	r.timers.close()
	r.clearImages()
	r.orderMu.Lock()
	r.notifications = nil
	r.steers = nil
	r.orderMu.Unlock()
	if invalid != nil {
		return errors.Join(err, fmt.Errorf("persist unusable context: %w", invalid))
	}
	return fmt.Errorf("context unusable after compaction: %w; inspect/export history or start/load another session", err)
}

func canonicalCompaction(messages []llm.Message) []llm.Message {
	result := append([]llm.Message(nil), messages...)
	for i := range result {
		result[i].State = nil
	}
	return result
}

func compactionInstruction() llm.Message {
	return llm.Message{Type: llm.TypeRuntimeInstruction, Content: prompts.Compaction}
}

func compactionInstructionTokens() int {
	return contextbuild.Tokens([]llm.Message{compactionInstruction()})
}

// codingInputFits leaves room for both the next coding response and a later
// summary instruction/response. It uses the same calibrated input estimate as
// coding, not a separately serialized transcript. Output reserves remain soft
// where the endpoint cannot cap generation.
func codingInputFits(input int, budget llm.Budget, headroom bool) bool {
	for _, reserve := range []int{compactionInstructionTokens(), budget.SummaryOutputAllowance} {
		if reserve < 0 || reserve >= budget.ContextLimit {
			return false
		}
		budget.ContextLimit -= reserve
	}
	return contextbuild.FitsInput(input, budget, headroom)
}

// preparedCompaction is shared by main, children and asides. Selection and
// summary input use exactly the admitted actor projection; durable history
// boundaries and handoff ownership remain with the caller.
type preparedCompaction struct {
	messages  []llm.Message
	retention contextbuild.Retention
	inputs    []llm.Message
	prefix    []llm.Message // Original structured history; replay survives summary inference only.
	at        time.Time
}

func prepareCompaction(messages []llm.Message, selection llm.Selection) (preparedCompaction, error) {
	p := preparedCompaction{messages: canonicalCompaction(messages), at: time.Now()}
	var err error
	p.retention, err = contextbuild.Retain(p.messages, selection.Model.Budget.RecentTokensMin, selection.Model.Budget.RecentTokensMax)
	if err != nil {
		return p, err
	}
	p.inputs, err = contextbuild.RetainedInputMessages(p.messages, p.retention.Inputs, p.at)
	if err != nil {
		return p, err
	}
	p.prefix = append([]llm.Message(nil), messages[:p.retention.Start]...)
	return p, nil
}

// compactionInput supplies the shared LLM-facing pipeline. freeze binds an
// ownership-specific durable cut after retention selection and before inference;
// it must not change the admitted message snapshot.
type compactionInput struct {
	actor, turn, focus string
	snapshot           contextbuild.InputSnapshot
	estimator          *contextbuild.Estimator
	force              bool
	freeze             func(preparedCompaction) error
}

type compactedInput struct {
	prepared preparedCompaction
	summary  string        // Raw model output for human-facing inspection.
	messages []llm.Message // Exact replacement: context, retained inputs and cycle tail.
}

// compactInput is the common compaction engine for main, children and asides.
// Callers own live-state resupply and commit; selection, inference and canonical
// replacement construction have one implementation.
func (r *Runtime) compactInput(ctx context.Context, input compactionInput) (compactedInput, error) {
	var result compactedInput
	if err := ctx.Err(); err != nil {
		return result, err
	}
	prepared, err := prepareCompaction(input.snapshot.Messages, input.snapshot.Selection)
	if err != nil {
		return result, err
	}
	if input.freeze != nil {
		if err := input.freeze(prepared); err != nil {
			return result, err
		}
	}
	input.snapshot.Messages = prepared.prefix
	summary, err := r.summarize(ctx, input.actor, input.turn, input.snapshot, input.estimator, input.focus, input.force)
	if err != nil {
		return result, err
	}
	messages := make([]llm.Message, 0, 1+len(prepared.inputs)+len(prepared.messages)-prepared.retention.Start)
	messages = append(messages, llm.NewCompactionContext(summary))
	messages = append(messages, prepared.inputs...)
	messages = append(messages, prepared.messages[prepared.retention.Start:]...)
	return compactedInput{prepared: prepared, summary: summary, messages: messages}, nil
}

func compactionFits(selection llm.Selection, system string, tools []llm.ToolDefinition, retained, notices []llm.Message, runtime *llm.Message) bool {
	messages := append(append([]llm.Message(nil), retained...), notices...)
	if runtime != nil {
		messages = append(messages, *runtime)
	}
	input := contextbuild.EstimateInput(contextbuild.InputSnapshot{Selection: selection, System: system, Tools: tools, Messages: messages})
	return codingInputFits(input.Input, selection.Model.Budget, true)
}

// summarize shares one request across main, coding children and asides using
// the compaction operation's context. Persistence follows the current continuation.
// force bypasses only the estimated input fit, never message validation or output limits.
func (r *Runtime) summarize(ctx context.Context, actor, turn string, snapshot contextbuild.InputSnapshot, estimator *contextbuild.Estimator, focus string, force bool) (text string, err error) {
	if estimator == nil || snapshot.Lineage == "" {
		return "", errors.New(prompts.SessionCompactionIdentity)
	}
	selection := snapshot.Selection
	projected, err := contextbuild.ProjectRetainedInputs(llm.ContextFor(selection, snapshot.Messages))
	if err != nil {
		return "", err
	}
	for _, message := range projected {
		if err := message.Validate(); err != nil {
			return "", err
		}
	}
	snapshot.Messages = projected
	if focus != "" {
		snapshot.Messages = append(snapshot.Messages, llm.Message{Type: llm.TypeRuntimeInstruction, Content: fmt.Sprintf(prompts.CompactionFocus, focus)})
	}
	snapshot.Messages = append(snapshot.Messages, compactionInstruction())
	if !force {
		snapshot.ReplayEstimator = r.Provider
		estimate := estimator.Estimate(snapshot)
		b := selection.Model.Budget
		b.OutputAllowance = b.SummaryOutputAllowance
		if !contextbuild.FitsInput(estimate.Input, b, false) {
			return "", &compactionCapacityError{fmt.Sprintf(prompts.SessionCompactionInputTooLarge, estimate.Input, b.OutputAllowance+b.EstimationMargin, b.ContextLimit)}
		}
	}
	r.routeMu.RLock()
	session := r.Current()
	request, err := r.Store.StartRequest(session, turn, actor, "compaction", selection)
	r.routeMu.RUnlock()
	if err != nil {
		return "", err
	}
	var usage *llm.Usage
	defer func() {
		status := "completed"
		if err != nil {
			status = "failed"
		}
		if errors.Is(err, context.Canceled) {
			status = "interrupted"
		}
		attempt := map[string]any{"status": status, "usage": usage}
		if err != nil {
			attempt["error"] = err.Error()
		}
		if failure := r.Store.FinishRequest(request, status, []any{attempt}); failure != nil {
			err = errors.Join(err, fmt.Errorf(prompts.SessionRecordCompactionResult, failure))
		}
	}()
	r.routeMu.RLock()
	id, err := r.Store.RecordSystemPrompt(r.Current(), turn, actor, request, snapshot.System)
	r.routeMu.RUnlock()
	if err != nil {
		return "", err
	}
	r.emit(Event{Kind: "system_prompt", Actor: actor, Text: "System prompt · compaction", EntryID: id})
	r.routeMu.RLock()
	ids, err := r.Store.RequestMessages(r.Current(), turn, "compaction", request, snapshot.Messages)
	r.routeMu.RUnlock()
	if err != nil {
		return "", err
	}
	// The full structured batch is durable, but only the final instruction needs
	// a UI row. Publishing every historical message would backpressure inference
	// on terminal redraws for large conversations.
	r.emit(Event{Kind: "message_placeholder", Actor: actor, Text: "Compaction instruction · inspect", EntryID: ids[len(ids)-1]})
	var summary strings.Builder
	err = r.Provider.Stream(ctx, llm.Request{ConversationID: snapshot.Lineage, Selection: selection, System: snapshot.System, Tools: snapshot.Tools, Messages: snapshot.Messages, NoTools: true, OutputTokens: selection.Model.Budget.SummaryOutputAllowance}, func(ev llm.StreamEvent) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		switch ev.Kind {
		case "call", "call_start":
			return errors.New(prompts.SessionCompactionNoTools)
		case "completed":
			usage = ev.Usage
		case "retry":
			return r.retryNotice(turn, actor, request, "compaction", ev.Retry)
		case "text":
			if summary.Len()+len(ev.Text) > 1<<20 {
				return &compactionCapacityError{prompts.SessionCompactionSummaryTooLarge}
			}
			summary.WriteString(ev.Text)
		}
		return nil
	})
	r.recordUsage(usage)
	r.routeMu.RLock()
	ids, failure := r.Store.RequestMessages(r.Current(), turn, "compaction", request, []llm.Message{{Type: llm.TypeAssistant, Content: summary.String()}})
	r.routeMu.RUnlock()
	if failure != nil {
		err = errors.Join(err, failure)
	} else {
		r.emit(Event{Kind: "message_placeholder", Actor: actor, Text: "Compaction reply · inspect", EntryID: ids[0]})
	}
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(summary.String())
	if text == "" {
		return "", errors.New(prompts.SessionCompactionEmptySummary)
	}
	return text, nil
}

// compactChild replaces only isolated actor input. Shared history, main cursor,
// file tips and undo ownership remain unchanged; the returned cursor forces full
// project context on the next request. Aside events never enter main context.
// pending is a persisted recovery warning required until coding succeeds.
func (r *Runtime) compactChild(ctx context.Context, task childTask, messages []llm.Message, cursor contextCursor, pending *llm.Message) ([]llm.Message, contextCursor, error) {
	ctx, cancel := context.WithTimeout(ctx, compactionTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, cursor, err
	}
	if task.actor == "" || task.tools == nil {
		return nil, cursor, errors.New(prompts.ChildCompactionIdentity)
	}
	system := childSystemTemplate
	if task.aside {
		system = asideSystemTemplate
	}
	var sourceSession string
	var sourceTip int64
	snapshot := contextbuild.InputSnapshot{Selection: task.selection, System: system, Tools: task.tools.Definitions(), Messages: messages, Lineage: task.actor}
	compacted, err := r.compactInput(ctx, compactionInput{
		actor: task.actor, turn: task.turn, snapshot: snapshot, estimator: task.estimator,
		freeze: func(preparedCompaction) error {
			r.routeMu.RLock()
			defer r.routeMu.RUnlock()
			sourceSession = r.Current()
			source, err := r.Store.Session(sourceSession)
			if err == nil {
				sourceTip = source.EntryTip
			}
			return err
		},
	})
	if err != nil {
		return nil, cursor, err
	}
	result := compacted.messages
	if task.aside {
		present := false
		for _, message := range result {
			if message.Role() == "developer" && message.Content == btwInstruction {
				present = true
				break
			}
		}
		if !present {
			updated := []llm.Message{result[0], {Type: llm.TypeRuntimeInstruction, Content: btwInstruction}}
			result = append(updated, result[1:]...)
		}
	}
	result, _ = contextbuild.AppendPendingMessage(result, pending)
	next := contextCursor{}
	if pending != nil {
		// Main compaction may have moved the warning behind a continuation cut
		// while summarization ran. Keep one durable warning in the active branch.
		if _, err = r.ensureRecovery(task.turn, task.actor, *pending); err != nil {
			return nil, cursor, err
		}
	}
	// Freeze pending delivery and runtime context through the handoff record.
	// Otherwise the replacement can report success but immediately fail admission
	// on notices which were already queued while summarization ran.
	r.routeMu.RLock()
	defer r.routeMu.RUnlock()
	r.orderMu.Lock()
	defer r.orderMu.Unlock()
	runtime, _, err := r.runtimeContextLocked(ctx, task.actor, task.selection, next)
	if err != nil {
		return nil, cursor, err
	}
	notices, _, err := r.childNoticeBatchLocked(task.assignment)
	if err != nil {
		return nil, cursor, err
	}
	if !compactionFits(task.selection, system, task.tools.Definitions(), result, notices, runtime) {
		if task.aside {
			return nil, cursor, &compactionCapacityError{"Read-only answer context still exceeds the model's budget after compaction. Retry /btw with a shorter, more focused question. The main conversation is unchanged."}
		}
		// This diagnostic reaches main through the failed child assignment.
		return nil, cursor, &compactionCapacityError{prompts.SessionCompactionHeadroom}
	}
	if err = ctx.Err(); err != nil {
		return nil, cursor, err
	}
	err = r.Store.RecordActorCompaction(r.Current(), task.turn, task.actor, sourceSession, sourceTip)
	if err != nil {
		return nil, cursor, err
	}
	task.estimator.Reset()
	if !task.aside {
		event, _ := json.Marshal(map[string]any{"type": "child_compacted", "child_id": task.actor, "turn_id": task.turn, "context_tokens_estimate": contextbuild.Tokens(result)})
		if err = r.queueNotificationLocked(string(event)); err != nil {
			return nil, cursor, err
		}
	}
	return result, next, nil
}
