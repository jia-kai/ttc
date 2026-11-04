package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	contextbuild "ttc/internal/context"
	"ttc/internal/history"
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
	if easy := r.EasyModelChoice(); easy != nil {
		selection = *easy
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
		attempt := history.RequestAttempt{Status: status, Usage: usage}
		if e != nil {
			attempt.Error = e.Error()
		} else if status != "completed" {
			attempt.Error = "naming request did not produce a valid title"
		}
		if err := r.Store.FinishRequest(request, history.RequestCompletion{Status: status, Attempts: []history.RequestAttempt{attempt}}); err != nil {
			r.namingFailure(ownerCtx, sessionID, turn, "record result: "+err.Error())
		}
	}()
	_, e = r.Store.RecordSystemPrompt(sessionID, turn, "main", request, system)
	if e != nil {
		r.namingFailure(ownerCtx, sessionID, turn, "save instructions: "+e.Error())
		return
	}
	user.Content, assistant.Content = text(user.Content), text(assistant.Content)
	if user.UserText != nil {
		bounded := text(*user.UserText)
		user.UserText = &bounded
	}
	userText, e := history.TranscriptMessage(user)
	if e != nil {
		r.namingFailure(ownerCtx, sessionID, turn, "render user input: "+e.Error())
		return
	}
	assistantText, e := history.TranscriptMessage(assistant)
	if e != nil {
		r.namingFailure(ownerCtx, sessionID, turn, "render assistant input: "+e.Error())
		return
	}
	message := llm.Message{Type: llm.TypeUserInput, Content: prompts.SessionNamingUser + userText + prompts.SessionNamingAssistant + assistantText}
	for i, call := range assistant.Calls {
		if i == 4 {
			message.Content += prompts.SessionNamingTruncated
			break
		}
		message.Content += prompts.SessionNamingTool + history.TranscriptCall(call.Name, call.Arguments)
	}
	if _, e := r.Store.RequestMessages(sessionID, turn, "naming", request, []llm.Message{message}); e != nil {
		r.namingFailure(ownerCtx, sessionID, turn, "save input: "+e.Error())
		return
	}
	var title strings.Builder
	e = r.stream(ctx, turn, "main", request, llm.Request{ConversationID: sessionID + "/naming", Selection: selection, System: system, Messages: []llm.Message{message}, NoTools: true, OutputTokens: namingOutputTokens}, func(ev llm.StreamEvent) error {
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
	if _, e := r.Store.RequestMessages(sessionID, turn, "naming", request, []llm.Message{{Type: llm.TypeAssistant, Content: title.String()}}); e != nil {
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

// compact runs manual compaction after Command checks context writability.
func (r *Runtime) compact(focus string, force bool) (string, error) {
	ctx, cancel := context.WithCancelCause(r.ctx)
	r.installForegroundCancel(cancel)
	defer func() { cancel(nil); r.mu.Lock(); r.activeCancel = nil; r.mu.Unlock() }()
	if force {
		r.emit(Event{Kind: "status", Text: "Forcing compaction: local summary input-size check disabled; provider limits and handoff checks still apply."})
	}
	result, err := r.compactContext(ctx, focus, r.CurrentSelection(), nil, force)
	if err != nil {
		return "", err
	}
	return result.Text, nil
}

// Continuation presents a committed compaction handoff. EntryTip freezes both
// history entries and grouped stream revisions in the event's SessionID before
// any subsequent coding response or foreground terminal status is committed.
type Continuation struct {
	EntryTip int64
	Text     string
}

// compactContext shares the manual and automatic handoff. The main loop is
// paused, but live children/jobs can append a tail until routeMu locks commit.
// An active main turn publishes its continuation before releasing that lock;
// idle manual compaction instead presents history through its command result.
// pending is a persisted recovery warning required until coding succeeds.
// force is a manual override of summary input-size preflight only.
func (r *Runtime) compactContext(ctx context.Context, focus string, selection llm.Selection, pending *llm.Message, force bool) (result *Continuation, err error) {
	session := r.Current()
	defer func() { err = r.compactionFailure(session, err) }()
	ctx, cancel := context.WithTimeout(ctx, compactionTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Record the boundary before naming/summary preparation so a process killed
	// before summary-request admission can still recover pending messages.
	if _, err := r.Store.Append(session, "", "main", "status", "", false, map[string]any{"type": "compaction_started", "force": force}); err != nil {
		return nil, fmt.Errorf("record compaction boundary: %w", err)
	}
	if r.namingDone != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-r.namingDone:
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// History freezes model input and owns its mapping to durable entries.
	view, e := r.Store.ReadCompaction(ctx, session)
	if e != nil {
		return nil, e
	}
	var plan history.CompactionPlan
	snapshot := contextbuild.InputSnapshot{Selection: selection, System: systemTemplate, Tools: r.Tools.Definitions(), Messages: view.Messages(), Lineage: session}
	compacted, e := r.compactInput(ctx, compactionInput{
		actor: "main", snapshot: snapshot, estimator: r.estimator, focus: focus, force: force,
		freeze: func(prepared preparedCompaction) error {
			var err error
			plan, err = view.Retain(prepared.retention.Start, prepared.retention.Inputs, prepared.at)
			return err
		},
	})
	if e != nil {
		return nil, e
	}
	publications := r.reservePublications(2)
	defer publications.release()
	r.routeMu.Lock()
	defer r.routeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result = &Continuation{}
	var automatic bool
	e = r.Workspace.Admit(ctx, func() error {
		r.orderMu.Lock()
		defer r.orderMu.Unlock()
		// Job/timer publishers use orderMu without routeMu. Resolve only after
		// owning both publication and workspace gates so their arrivals join the
		// capacity-checked tail rather than invalidating a sound handoff.
		resolved, e := r.Store.ResolveCompaction(ctx, plan, compacted.messages[0], pending)
		if e != nil {
			return e
		}
		assembled := resolved.Messages()
		cursor := contextCursor{}
		contextMessage, _, e := r.runtimeContextLocked(ctx, "main", selection, cursor)
		if e != nil {
			return e
		}
		notices, _, e := r.notifications.batch(r.projectRuntimeNotice, 0)
		if e != nil {
			return e
		}
		notices = append(notices, r.steeringMessagesLocked()...)
		if !compactionFits(selection, systemTemplate, r.Tools.Definitions(), assembled, notices, contextMessage) {
			return &compactionCapacityError{"Compacted context still exceeds this model's input budget. The current context was not replaced, and existing jobs/timers remain live. Select a larger-context model with /model, or save a concise handoff and start a new session with /new (which cancels session-owned work)."}
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		v, e := r.Store.CommitCompaction(ctx, resolved)
		if e != nil {
			return e
		}
		r.current = v.ID
		automatic = r.activeTurn != ""
		r.estimator.Reset()
		r.mainContext = contextCursor{}
		input := append(append([]llm.Message(nil), assembled...), notices...)
		if contextMessage != nil {
			input = append(input, *contextMessage)
		}
		r.usage = estimateUsage(selection, systemTemplate, r.Tools.Definitions(), input)
		// CommitCompaction returns the committed tip while routing remains locked. No
		// later coding, child or stream revision may extend this replay cut.
		result.EntryTip = v.EntryTip
		result.Text = "## Compacted · " + v.Name + "\n\n" + compacted.summary
		return nil
	})
	if e != nil {
		return nil, e
	}
	r.emitReserved(publications, Event{Kind: "usage"})
	if automatic {
		// Fence post-handoff child publications as well as later main work.
		// Publishing after routeMu unlock would allow a later event to appear
		// before this replay, which would then erase its already-consumed row.
		r.emitReserved(publications, Event{Kind: "continuation", Continuation: result})
	}
	return result, nil
}

func (r *Runtime) stopNaming() {
	if r.namingCancel != nil {
		r.namingCancel()
	}
	if r.namingDone != nil {
		<-r.namingDone
	}
}
