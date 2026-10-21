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

const summaryInstructions = prompts.Compaction

// compactionTimeout sets the cooperative deadline for the entire operation.
// Earlier owning-context deadlines apply; synchronous storage may delay return.
const compactionTimeout = 10 * time.Minute

// Recoverable failures leave the existing context usable. Unknown errors,
// malformed summaries and broken persistence conservatively invalidate it.
func recoverableCompaction(err error) bool {
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

// summaryTranscript preserves model text and raw tool payloads without display
// expansion, replay state or attachment bytes. Retained human inputs include
// typed original-source/commit and compaction metadata. Durable history keeps
// exact records; summarization uses only the compacting actor's admitted messages.
func summaryTranscript(messages []llm.Message) (string, error) {
	var out strings.Builder
	for _, message := range messages {
		content, err := message.ModelContent()
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&out, prompts.SessionSummaryMessageHeader, message.Role())
		if message.CallID != "" {
			fmt.Fprintf(&out, prompts.SessionSummaryCallID, message.CallID)
		}
		out.WriteString("\n")
		if message.RetainedAtMS > 0 {
			source := message.InputSource
			if source == "" {
				source = "normal"
			}
			metadata, _ := json.Marshal(struct {
				Source       string `json:"input_source"`
				OriginalAtMS int64  `json:"input_time_ms"`
				RetainedAtMS int64  `json:"retained_at_ms,omitempty"`
			}{source, message.InputTimeMS, message.RetainedAtMS})
			out.Write(metadata)
			out.WriteByte('\n')
		}
		out.WriteString(content)
		out.WriteByte('\n')
		for _, call := range message.Calls {
			fmt.Fprintf(&out, prompts.SessionSummaryToolCall, call.ID, call.Name, call.Arguments)
		}
		for _, file := range message.Files {
			fmt.Fprintf(&out, prompts.SessionSummaryBinaryAttachment, file.MIMEType, file.Path)
		}
	}
	return out.String(), nil
}

func compactionHandoff(summary string) string {
	return fmt.Sprintf(prompts.CompactionHandoff, summary)
}

// preparedCompaction is shared by main, children and asides. Selection and
// summary input use exactly the admitted actor projection; durable history
// boundaries and handoff ownership remain with the caller.
type preparedCompaction struct {
	messages   []llm.Message
	retention  contextbuild.Retention
	inputs     []llm.Message
	transcript string
	at         time.Time
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
	p.transcript, err = summaryTranscript(p.messages[:p.retention.Start])
	return p, err
}

func compactionFits(selection llm.Selection, system string, tools []llm.ToolDefinition, retained, notices []llm.Message, runtime *llm.Message) error {
	messages := append(append([]llm.Message(nil), retained...), notices...)
	if runtime != nil {
		messages = append(messages, *runtime)
	}
	if !contextbuild.Fits(selection, system, tools, messages, true) {
		return errors.New(prompts.SessionCompactionHeadroom)
	}
	return nil
}

// summarize shares one request across main, coding children and asides using
// the compaction operation's context. Persistence follows the current continuation.
func (r *Runtime) summarize(ctx context.Context, actor, turn string, selection llm.Selection, prefix, focus string) (text string, err error) {
	input := llm.Message{Type: llm.TypeUserInput, Content: fmt.Sprintf(prompts.CompactionInput, focus, prefix)}
	budget := selection
	budget.Model.Budget.OutputAllowance = budget.Model.Budget.SummaryOutputAllowance
	if !contextbuild.Fits(budget, summaryInstructions, nil, []llm.Message{input}, false) {
		b := budget.Model.Budget
		return "", fmt.Errorf(prompts.SessionCompactionInputTooLarge, contextbuild.Estimate(summaryInstructions)+contextbuild.Tokens([]llm.Message{input}), b.OutputAllowance+b.EstimationMargin, b.ContextLimit)
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
	id, err := r.Store.RecordSystemPrompt(r.Current(), turn, actor, request, summaryInstructions)
	r.routeMu.RUnlock()
	if err != nil {
		return "", err
	}
	r.emit(Event{Kind: "system_prompt", Actor: actor, Text: "System prompt · compaction", EntryID: id})
	persist := func(message llm.Message, label string) error {
		r.routeMu.RLock()
		id, e := r.Store.RequestMessage(r.Current(), turn, "compaction", message.Role(), request, message)
		r.routeMu.RUnlock()
		if e == nil {
			r.emit(Event{Kind: "message_placeholder", Actor: actor, Text: label, EntryID: id})
		}
		return e
	}
	if err = persist(input, "Compaction input · inspect"); err != nil {
		return "", err
	}
	var summary strings.Builder
	conversation := session + "/compaction"
	if actor != "main" {
		conversation = session + "/" + actor + "/compaction"
	}
	err = r.Provider.Stream(ctx, llm.Request{ConversationID: conversation, Selection: selection, System: summaryInstructions, Messages: []llm.Message{input}, NoTools: true, OutputTokens: selection.Model.Budget.SummaryOutputAllowance}, func(ev llm.StreamEvent) error {
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
				return errors.New(prompts.SessionCompactionSummaryTooLarge)
			}
			summary.WriteString(ev.Text)
		}
		return nil
	})
	r.recordUsage(usage)
	if failure := persist(llm.Message{Type: llm.TypeAssistant, Content: summary.String()}, "Compaction reply · inspect"); failure != nil {
		err = errors.Join(err, failure)
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
	prepared, err := prepareCompaction(messages, task.selection)
	if err != nil {
		return nil, cursor, err
	}
	r.routeMu.RLock()
	sourceSession := r.Current()
	source, err := r.Store.Session(sourceSession)
	r.routeMu.RUnlock()
	if err != nil {
		return nil, cursor, err
	}
	summary, err := r.summarize(ctx, task.actor, task.turn, task.selection, prepared.transcript, "")
	if err != nil {
		return nil, cursor, err
	}
	summary = compactionHandoff(summary)
	result := []llm.Message{{Type: llm.TypeAssistant, Content: summary}}
	result = append(result, prepared.inputs...)
	result = append(result, prepared.messages[prepared.retention.Start:]...)
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
	system := childSystemTemplate
	if task.aside {
		system = asideSystemTemplate
	}
	runtime, _, err := r.runtimeContext(ctx, task.actor, task.selection, next)
	if err != nil {
		return nil, cursor, err
	}
	if err = compactionFits(task.selection, system, task.tools.Definitions(), result, nil, runtime); err != nil {
		return nil, cursor, err
	}
	if err = ctx.Err(); err != nil {
		return nil, cursor, err
	}
	if pending != nil {
		// Main compaction may have moved the warning behind a continuation cut
		// while summarization ran. Keep one durable warning in the active branch.
		if _, err = r.ensureRecovery(task.turn, task.actor, *pending); err != nil {
			return nil, cursor, err
		}
	}
	r.routeMu.RLock()
	err = r.Store.RecordActorCompaction(r.Current(), task.turn, task.actor, sourceSession, source.EntryTip)
	r.routeMu.RUnlock()
	if err != nil {
		return nil, cursor, err
	}
	task.estimator.Reset()
	if !task.aside {
		event, _ := json.Marshal(map[string]any{"type": "child_compacted", "child_id": task.actor, "turn_id": task.turn, "context_tokens_estimate": contextbuild.Tokens(result)})
		if err = r.queueNotification(string(event)); err != nil {
			return nil, cursor, err
		}
	}
	return result, next, nil
}
