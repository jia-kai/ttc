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
	"ttc/internal/provider"
	"ttc/internal/render"
)

const summaryInstructions = prompts.Compaction

// Recoverable failures leave the existing context usable. Unknown errors,
// malformed summaries and broken persistence conservatively invalidate it.
func recoverableCompaction(err error) bool {
	if _, ok := err.(*provider.TransientError); ok {
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

func canonicalCompaction(messages []provider.Message) []provider.Message {
	result := append([]provider.Message(nil), messages...)
	for i := range result {
		result[i].State = nil
	}
	return result
}

// summaryTranscript preserves model text and raw tool payloads without display
// expansion, replay state or attachment bytes. Durable archives keep the exact
// records; summarization uses only the compacting actor's admitted messages.
func summaryTranscript(messages []provider.Message) string {
	var out strings.Builder
	for _, message := range messages {
		fmt.Fprintf(&out, "\n### %s", message.Role)
		if message.CallID != "" {
			fmt.Fprintf(&out, " · %s", message.CallID)
		}
		out.WriteString("\n")
		out.WriteString(message.Content)
		out.WriteByte('\n')
		for _, call := range message.Calls {
			fmt.Fprintf(&out, "Tool call %s · %s\n%s\n", call.ID, call.Name, call.Arguments)
		}
		for _, file := range message.Files {
			fmt.Fprintf(&out, "Binary attachment (%s): %s\n", file.MIMEType, file.Path)
		}
	}
	return out.String()
}

func compactionLinks(summary, archive, exact string) string {
	return fmt.Sprintf(prompts.CompactionLinks, summary, archive, exact)
}

func compactionFits(selection provider.Selection, system string, tools []provider.ToolDefinition, retained, notices []provider.Message, runtime *provider.Message) error {
	messages := append(append([]provider.Message(nil), retained...), notices...)
	if runtime != nil {
		messages = append(messages, *runtime)
	}
	if !contextbuild.Fits(selection, system, tools, messages, true) {
		return errors.New("compaction summary, retained history (including unread binary results) and pending input/events exceed context headroom; use smaller files or narrower input")
	}
	return nil
}

// summarize performs one bounded request shared by main, coding children and
// asides. Request persistence follows the current continuation at each boundary.
func (r *Runtime) summarize(ctx context.Context, actor, turn string, selection provider.Selection, prefix, focus string) (text string, err error) {
	input := provider.Message{Role: "user", Content: fmt.Sprintf(prompts.CompactionInput, focus, prefix)}
	budget := selection
	budget.Model.Budget.OutputAllowance = budget.Model.Budget.SummaryOutputAllowance
	if !contextbuild.Fits(budget, summaryInstructions, nil, []provider.Message{input}, false) {
		b := budget.Model.Budget
		return "", fmt.Errorf("compaction input exceeds model context (estimated input %d + output/margin reserve %d, limit %d tokens); no summary was requested", contextbuild.Estimate(summaryInstructions)+contextbuild.Tokens([]provider.Message{input}), b.OutputAllowance+b.EstimationMargin, b.ContextLimit)
	}
	r.routeMu.RLock()
	session := r.Current()
	request, err := r.Store.StartRequest(session, turn, actor, "compaction", selection)
	r.routeMu.RUnlock()
	if err != nil {
		return "", err
	}
	var usage *provider.Usage
	defer func() {
		status := "completed"
		if err != nil {
			status = "failed"
		}
		if errors.Is(err, context.Canceled) {
			status = "interrupted"
		}
		if failure := r.Store.FinishRequest(request, status, []any{map[string]any{"status": status, "usage": usage}}); failure != nil {
			err = errors.Join(err, fmt.Errorf("record compaction result: %w", failure))
		}
	}()
	r.routeMu.RLock()
	id, err := r.Store.RecordSystemPrompt(r.Current(), turn, actor, request, summaryInstructions)
	r.routeMu.RUnlock()
	if err != nil {
		return "", err
	}
	r.emit(Event{Kind: "system_prompt", Actor: actor, Text: "System prompt · compaction", EntryID: id})
	persist := func(message provider.Message, label string) error {
		r.routeMu.RLock()
		id, e := r.Store.RequestMessage(r.Current(), turn, "compaction", message.Role, request, message)
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
	err = r.Provider.Stream(ctx, provider.Request{ConversationID: conversation, Selection: selection, System: summaryInstructions, Messages: []provider.Message{input}, NoTools: true, OutputTokens: selection.Model.Budget.SummaryOutputAllowance}, func(ev provider.StreamEvent) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		switch ev.Kind {
		case "call", "call_start":
			return errors.New("compaction response must not call tools")
		case "completed":
			usage = ev.Usage
		case "retry":
			return r.retryNotice(turn, actor, request, "compaction", ev.Retry)
		case "text":
			if summary.Len()+len(ev.Text) > 1<<20 {
				return errors.New("compaction summary exceeds 1 MiB")
			}
			summary.WriteString(ev.Text)
		}
		return nil
	})
	r.recordUsage(usage)
	if failure := persist(provider.Message{Role: "assistant", Content: summary.String()}, "Compaction reply · inspect"); failure != nil {
		err = errors.Join(err, failure)
	}
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(summary.String())
	if text == "" {
		return "", errors.New("empty compaction summary")
	}
	return text, nil
}

// compactChild replaces only isolated actor input. Shared history, main cursor,
// file tips and undo ownership remain unchanged; the returned cursor forces full
// project context on the next request. Aside events never enter main context.
func (r *Runtime) compactChild(ctx context.Context, task childTask, messages []provider.Message, cursor contextCursor) ([]provider.Message, contextCursor, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, cursor, err
	}
	if task.actor == "" || task.tools == nil {
		return nil, cursor, errors.New("child compaction requires actor identity and tool registry")
	}
	canonical := canonicalCompaction(messages)
	compactedAt := time.Now()
	retention, err := contextbuild.Retain(canonical, task.selection.Model.Budget.RecentTokensMin, task.selection.Model.Budget.RecentTokensMax)
	if err != nil {
		return nil, cursor, err
	}
	retainedInputs, err := contextbuild.RetainedInputMessages(canonical, retention.Inputs, compactedAt)
	if err != nil {
		return nil, cursor, err
	}
	// Exact provider replay and attachment bytes stay in the private sidecar.
	markdown := "# Child context · " + render.Inline(task.actor) + "\n\n" + childTranscript(messages)
	r.routeMu.RLock()
	archive, err := r.Store.ArchiveMessages(r.Current(), task.actor, markdown, messages)
	r.routeMu.RUnlock()
	if err != nil {
		return nil, cursor, err
	}
	records := archive + ".jsonl"
	summary, err := r.summarize(ctx, task.actor, task.turn, task.selection, summaryTranscript(canonical[:retention.Start]), "")
	if err != nil {
		return nil, cursor, err
	}
	summary = compactionLinks(summary, archive, records)
	result := []provider.Message{{Role: "assistant", Content: summary}}
	result = append(result, retainedInputs...)
	result = append(result, canonical[retention.Start:]...)
	if task.aside {
		present := false
		for _, message := range result {
			if message.Role == "developer" && message.Content == btwInstruction {
				present = true
				break
			}
		}
		if !present {
			updated := []provider.Message{result[0], {Role: "developer", Runtime: true, Content: btwInstruction}}
			result = append(updated, result[1:]...)
		}
	}
	next := cursor
	next.project = ""
	next.snapshot = ""
	system := childSystemTemplate
	if task.aside {
		system = systemTemplate
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
	if !task.aside {
		event, _ := json.Marshal(map[string]any{"type": "child_compacted", "child_id": task.actor, "turn_id": task.turn, "archive": archive, "records": records, "context_tokens_estimate": contextbuild.Tokens(result)})
		if err = r.queueNotification(string(event)); err != nil {
			return nil, cursor, err
		}
	}
	return result, next, nil
}

func childTranscript(messages []provider.Message) string {
	calls := map[string]provider.ToolCall{}
	finished := map[string]bool{}
	for _, message := range messages {
		for _, call := range message.Calls {
			calls[call.ID] = call
		}
		if message.Role == "tool" {
			finished[message.CallID] = true
		}
	}
	var out strings.Builder
	for _, message := range messages {
		if message.Role == "tool" {
			if call, ok := calls[message.CallID]; ok && json.Valid([]byte(message.Content)) {
				fmt.Fprintf(&out, "### Tool · %s\n\n%s\n\n", render.Inline(call.Name), render.Tool(call.Name, call.Arguments, json.RawMessage(message.Content)).ExportText())
				// Live inspectors recover captures through JobDetail. An archive
				// has no live handle, so retain the model-visible stream tails here.
				var result map[string]json.RawMessage
				if json.Unmarshal([]byte(message.Content), &result) == nil && result["job_id"] != nil {
					for _, stream := range []string{"stdout", "stderr"} {
						var text string
						if json.Unmarshal(result[stream], &text) == nil && text != "" {
							fmt.Fprintf(&out, "#### %s\n\n%s\n", stream, render.Fence(text, "text"))
						}
					}
				}
				continue
			}
		}
		if message.Content != "" {
			body := render.Clean(message.Content)
			if json.Valid([]byte(message.Content)) {
				body = render.Fence(message.Content, "json")
			}
			fmt.Fprintf(&out, "### %s\n\n%s\n\n", render.Inline(message.Role), body)
		}
		for _, call := range message.Calls {
			if !finished[call.ID] {
				out.WriteString(render.Tool(call.Name, call.Arguments, nil).Detail + "\n\n")
			}
		}
		for _, file := range message.Files {
			fmt.Fprintf(&out, "Binary file: %s\n\n", render.Inline(file.Path))
		}
	}
	return out.String()
}
