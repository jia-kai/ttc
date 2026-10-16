package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/prompts"
	"ttc/internal/render"
	"unicode"
	"unicode/utf8"
)

const (
	namingOutputTokens = 32 // Includes reasoning tokens.
	namingTimeout      = 15 * time.Second
)

func httpClient() *http.Client { return &http.Client{Timeout: 120 * time.Second} }

// startNaming claims one background request at the first settled tool batch,
// or completed response without tools. The running coding turn remains usable.
func (r *Runtime) startNaming(turn string, selection llm.Selection, user, assistant llm.Message) {
	sessionID := r.Current()
	claimed, err := r.Store.ClaimSessionNaming(r.namingCtx, sessionID, turn)
	if err != nil {
		r.namingFailure(r.namingCtx, sessionID, turn, "claim: "+err.Error())
		return
	}
	if !claimed {
		return
	}
	done := make(chan struct{})
	r.namingDone = done
	ctx := r.namingCtx
	go func() {
		defer close(done)
		r.name(ctx, sessionID, selection, turn, user, assistant)
	}()
}

func (r *Runtime) namingFailure(ctx context.Context, sessionID, turn, reason string) {
	if ctx.Err() != nil {
		return // An abandoned session's canceled naming must not notify its replacement.
	}
	text := "Session naming failed: " + render.Clean(reason)
	id, _ := r.Store.Append(sessionID, turn, "main", "status", "", false, map[string]string{"type": "naming_error", "text": text})
	r.emit(Event{Kind: "status", Text: text, EntryID: id, SessionID: sessionID})
}

func (r *Runtime) name(ctx context.Context, sessionID string, selection llm.Selection, turn string, user, assistant llm.Message) {
	ownerCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, namingTimeout)
	defer cancel()
	text := func(s string) string {
		if len(s) > 4096 {
			s = s[:4096]
			for !utf8.ValidString(s) {
				s = s[:len(s)-1]
			}
			return s + prompts.SessionNamingTruncated
		}
		return s
	}
	system := prompts.Naming
	request, e := r.Store.StartRequest(sessionID, turn, "main", "naming", selection)
	if e != nil {
		r.namingFailure(ownerCtx, sessionID, turn, "start request: "+e.Error())
		return
	}
	var usage *llm.Usage
	status := "failed"
	defer func() {
		if ownerCtx.Err() != nil {
			status = "interrupted"
		}
		attempt := map[string]any{"status": status, "usage": usage}
		if e != nil {
			attempt["error"] = e.Error()
		} else if status != "completed" {
			attempt["error"] = "naming request did not produce a valid title"
		}
		if err := r.Store.FinishRequest(request, status, []any{attempt}); err != nil {
			r.namingFailure(ownerCtx, sessionID, turn, "record result: "+err.Error())
		}
	}()
	id, e := r.Store.RecordSystemPrompt(sessionID, turn, "main", request, system)
	if e != nil {
		r.namingFailure(ownerCtx, sessionID, turn, "save instructions: "+e.Error())
		return
	}
	r.emit(Event{Kind: "system_prompt", Text: "System prompt · session naming", EntryID: id})
	message := llm.Message{Type: llm.TypeUserInput, Content: text(user.Content) + "\n\n" + text(assistant.Content)}
	for i, call := range assistant.Calls {
		if i == 4 {
			break
		}
		message.Content += prompts.SessionNamingTool + render.Clean(call.Name)
	}
	if id, e := r.Store.RequestMessage(sessionID, turn, "naming", "user", request, message); e == nil {
		r.emit(Event{Kind: "message_placeholder", Text: "Session naming input · inspect", EntryID: id})
	} else {
		r.namingFailure(ownerCtx, sessionID, turn, "save input: "+e.Error())
		return
	}
	var title strings.Builder
	e = r.Provider.Stream(ctx, llm.Request{ConversationID: sessionID + "/naming", Selection: selection, System: system, Messages: []llm.Message{message}, NoTools: true, OutputTokens: namingOutputTokens}, func(ev llm.StreamEvent) error {
		if ev.Kind == "completed" {
			usage = ev.Usage
		}
		if ev.Kind == "retry" {
			return r.retryNotice(turn, "main", request, "naming", ev.Retry)
		}
		if ev.Kind == "text" {
			if title.Len()+len(ev.Text) > 256 {
				return errors.New("title exceeds 256-byte limit")
			}
			title.WriteString(ev.Text)
		}
		return nil
	})
	r.recordUsage(usage)
	if id, e := r.Store.RequestMessage(sessionID, turn, "naming", "assistant", request, llm.Message{Type: llm.TypeAssistant, Content: title.String()}); e == nil {
		r.emit(Event{Kind: "message_placeholder", Text: "Session naming reply · inspect", EntryID: id})
	} else {
		r.namingFailure(ownerCtx, sessionID, turn, "save reply: "+e.Error())
		return
	}
	v := strings.Trim(title.String(), " \n\t\"'`*#")
	words := strings.Fields(v)
	if e == nil && len(words) >= 3 && len(words) <= 6 && utf8.RuneCountInString(v) <= 60 && strings.IndexFunc(v, unicode.IsControl) < 0 {
		var updated bool
		updated, e = r.Store.SetAutomaticSessionName(ctx, sessionID, v)
		if e == nil {
			status = "completed"
			if updated {
				r.emit(Event{Kind: "session_name", Text: v, SessionID: sessionID})
			}
		}
	}
	if status != "completed" {
		reason := "provider returned an invalid title (expected 3–6 words, at most 60 characters)"
		if e != nil {
			reason = e.Error()
		}
		r.namingFailure(ownerCtx, sessionID, turn, reason)
	}
}
func (r *Runtime) compact(focus string) (string, error) {
	ctx, cancel := context.WithCancel(r.ctx)
	r.mu.Lock()
	r.activeCancel = cancel
	r.mu.Unlock()
	defer func() { cancel(); r.mu.Lock(); r.activeCancel = nil; r.mu.Unlock() }()
	return r.compactContext(ctx, focus, r.CurrentSelection(), nil)
}

// compactContext shares the manual and automatic handoff. The main loop is
// paused, but live children/jobs can append a tail until routeMu locks commit.
// pending is a persisted recovery warning required until coding succeeds.
func (r *Runtime) compactContext(ctx context.Context, focus string, selection llm.Selection, pending *llm.Message) (result string, err error) {
	session := r.Current()
	defer func() { err = r.compactionFailure(session, err) }()
	ctx, cancel := context.WithTimeout(ctx, compactionTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Record the boundary before naming/summary preparation so a process killed
	// before summary-request admission can still recover pending messages.
	if _, err := r.Store.Append(session, "", "main", "status", "", false, map[string]string{"type": "compaction_started"}); err != nil {
		return "", fmt.Errorf("record compaction boundary: %w", err)
	}
	if r.namingDone != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-r.namingDone:
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	messages, e := r.Store.Messages(r.Current())
	if e != nil {
		return "", e
	}
	prepared, e := prepareCompaction(messages, selection)
	if e != nil {
		return "", e
	}
	retention := prepared.retention
	entries, e := r.Store.Branch(r.Current(), 0)
	if e != nil {
		return "", e
	}
	if len(entries) == 0 {
		return "", errors.New("nothing to compact")
	}
	visible := 0
	retainFrom := entries[len(entries)-1].ID + 1
	inputIDs := make([]int64, 0, len(retention.Inputs))
	for _, v := range entries {
		if !v.Visible {
			continue
		}
		if len(inputIDs) < len(retention.Inputs) && visible == retention.Inputs[len(inputIDs)] {
			inputIDs = append(inputIDs, v.ID)
		}
		if visible == retention.Start {
			retainFrom = v.ID
			break
		}
		visible++
	}
	if visible < retention.Start || len(inputIDs) != len(retention.Inputs) {
		return "", errors.New("no safe compaction cut")
	}
	// Summarize the same actor projection used for admission and retention.
	// UI-only child records and expanded tool presentations remain in history.
	text, e := r.summarize(ctx, "main", "", selection, prepared.transcript, focus)
	if e != nil {
		return "", e
	}
	text = compactionHandoff(text)
	r.routeMu.Lock()
	defer r.routeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Re-read the frozen suffix and any committed child tail under the same lock
	// as handoff. A tail which no longer fits aborts without changing sessions.
	currentEntries, e := r.Store.Branch(r.Current(), 0)
	if e != nil {
		return "", e
	}
	currentMessages, e := r.Store.Messages(r.Current())
	if e != nil {
		return "", e
	}
	assembled := []llm.Message{{Type: llm.TypeAssistant, Content: text}}
	assembled = append(assembled, prepared.inputs...)
	visible = 0
	for _, entry := range currentEntries {
		if !entry.Visible {
			continue
		}
		if visible >= len(currentMessages) {
			return "", errors.New("compaction history changed during projection")
		}
		message := currentMessages[visible]
		message.State = nil
		visible++
		if entry.ID >= retainFrom {
			assembled = append(assembled, message)
		}
	}
	if visible != len(currentMessages) {
		return "", errors.New("invalid compaction projection")
	}
	assembled, _ = contextbuild.AppendPendingMessage(assembled, pending)
	var name string
	e = r.Workspace.Admit(ctx, func() error {
		r.orderMu.Lock()
		defer r.orderMu.Unlock()
		cursor := r.mainContext
		cursor.project = ""
		cursor.snapshot = ""
		contextMessage, _, e := r.runtimeContextLocked(ctx, "main", selection, cursor)
		if e != nil {
			return e
		}
		notices := append(append([]llm.Message(nil), r.notifications...), r.steeringMessagesLocked()...)
		if e = compactionFits(selection, systemTemplate, r.Tools.Definitions(), assembled, notices, contextMessage); e != nil {
			return e
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		v, e := r.Store.Continue(r.current, text, entries[len(entries)-1].ID, retainFrom, inputIDs, prepared.at, pending)
		if e != nil {
			return e
		}
		r.current = v.ID
		r.mainContext.project = ""
		r.mainContext.snapshot = ""
		input := append(append([]llm.Message(nil), assembled...), notices...)
		if contextMessage != nil {
			input = append(input, *contextMessage)
		}
		r.usage = estimateUsage(selection, systemTemplate, r.Tools.Definitions(), input)
		name = v.Name
		return nil
	})
	if e != nil {
		return "", e
	}
	r.emit(Event{Kind: "usage"})
	return "## Compacted · " + name + "\n\n" + text, nil
}

func (r *Runtime) stopNaming() {
	if r.namingCancel != nil {
		r.namingCancel()
	}
	if r.namingDone != nil {
		<-r.namingDone
	}
}
