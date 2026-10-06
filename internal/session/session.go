// Package session owns one active conversation and its transient runtime work.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/history"
	"ttc/internal/jobs"
	"ttc/internal/provider"
	"ttc/internal/render"
	"ttc/internal/skills"
	"ttc/internal/tool"
	"ttc/internal/workspace"
)

// Event contains a view update, never authentication state. EntryID makes every record inspectable.
type Event struct {
	Generation uint64 // Live-state owner; compaction preserves it, explicit resets advance it.
	Kind, Text string
	SessionID  string // Runtime conversation at emission; the frontend discards stale-view events.
	EntryID    int64
	RequestID  int64           // Producing model request for streamed/completed assistant text.
	Actor      string          // Producing actor for any event; empty means the main actor.
	CallID     string          // Stable tool-card identity across transient updates and the final record.
	PendingKey string          // Request-scoped streamed announcement to replace when intent is committed.
	JobID      string          // Optional live job for bounded inspector polling; never revived from history.
	Detail     string          // Bounded detail for a transient tool update; final detail is loaded from history.
	Human      bool            // True only for a submitted human instruction.
	Image      *ImageSnapshot  // Immutable image_show snapshot, not a live interaction handle.
	Question   *QuestionForm   // Snapshot for question lifecycle events; never persisted as a live handle.
	Retry      *provider.Retry // Foreground retry backoff metadata; nil for background session naming.
}

// Runtime owns exactly one main session, and joins transient work before switching it.
// Run, RunInput, and Command are called serially by the frontend;
// Interrupt and RequestModel are concurrent-safe.
type Runtime struct {
	Store             *history.Store
	Workspace         *workspace.Manager
	Provider          provider.Provider
	selection         provider.Selection
	pendingModel      *provider.Selection
	Skills            *skills.Catalog
	Tools             *tool.Registry
	searchConfig      tool.WebSearchConfig // Operator settings, excluded from model context/history.
	Jobs              *jobs.Manager
	Emit              func(Event)
	ctx               context.Context
	cancel            context.CancelFunc
	mu                sync.Mutex
	runMu             sync.Mutex // Serializes main foreground turns and idle lifecycle commands.
	orderMu           sync.Mutex // Serializes event publication and immutable request admission.
	fatalCompaction   string     // Main context failure, including a failed invalidation write.
	orderError        error      // Persistence failure prevents inference on an incomplete live snapshot.
	publishedJobs     map[string]jobs.Snapshot
	publishedTimers   map[string]wakeup
	publishedChildren map[string]tool.ChildView
	retentionStop     func()
	children          map[string]*codingChild // Owned by childStartMu; live handles never restored.
	childStartMu      sync.Mutex              // Serializes child capacity checks and admission, never child execution.
	routeMu           sync.RWMutex            // Keeps child commits/file mutations within the current continuation.
	current           string
	persisted         bool   // False until the first user turn/message is committed atomically.
	generation        uint64 // Advances on explicit transient resets, never compaction.
	activeCancel      context.CancelFunc
	notifications     []provider.Message
	steers            []contextbuild.Input // Original transient human inputs, admitted only at a model boundary.
	activeTurn        string               // Main inference turn, distinct from steering undo checkpoints.
	AutoName          bool
	timers            *wakeups
	questions         questions
	images            imageInteractions
	usage             ContextUsage
	reported          *ReportedUsage     // Protected by mu; cleared with the main session's transient state.
	totals            UsageTotals        // All inference usage since construction or /new, protected by mu.
	mainContext       contextCursor      // Owned by serial Run/Command; reset on explicit session changes.
	mainPrefix        []provider.Message // Latest balanced main request input, immutable after publication under mu.
	prefixSelection   provider.Selection
	prefixTurn        string
	namingCtx         context.Context
	namingCancel      context.CancelFunc
	namingDone        chan struct{} // Serial main-loop ownership; closed after the one naming task finishes.
}

// New constructs an active runtime. An empty session ID starts an in-memory
// blank conversation; a nonempty ID must refer to an already persisted session.
// Loaded compaction-boundary notifications are queued before any work starts.
func New(ctx context.Context, store *history.Store, w *workspace.Manager, p provider.Provider, selection provider.Selection, session string, catalog *skills.Catalog, search tool.WebSearchConfig, emit func(Event)) (*Runtime, error) {
	ctx, cancel := context.WithCancel(ctx)
	persisted := session != ""
	var notices []provider.Message
	if persisted {
		var err error
		notices, err = store.PendingRecoveryNotifications(ctx, session)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("read recovered notifications: %w", err)
		}
	}
	if !persisted {
		session = history.NewID("session")
	}
	r := &Runtime{Store: store, Workspace: w, Provider: p, selection: selection, Skills: catalog, searchConfig: search, Emit: emit, ctx: ctx, cancel: cancel, current: session, persisted: persisted, AutoName: true}
	r.resetTransient()
	r.notifications = notices
	r.retentionStop = r.startRetention()
	return r, nil
}
func (r *Runtime) resetTransient() {
	r.resetChildren()
	r.orderMu.Lock()
	r.publishedJobs = map[string]jobs.Snapshot{}
	r.publishedTimers = map[string]wakeup{}
	r.publishedChildren = map[string]tool.ChildView{}
	r.orderError = nil
	r.notifications = nil
	r.steers = nil
	r.orderMu.Unlock()
	r.mu.Lock()
	r.fatalCompaction = ""
	r.usage = ContextUsage{}
	r.reported = nil
	r.mainContext = contextCursor{}
	r.mainPrefix = nil
	r.prefixTurn = ""
	r.generation++
	r.mu.Unlock()
	r.namingCtx, r.namingCancel = context.WithCancel(r.ctx)
	r.namingDone = nil
	r.Tools = tool.NewRegistry()
	r.Tools.Detail = func(x tool.Execution, b []byte) (string, error) { return r.Store.Artifact(x.SessionID, "details", b) }
	r.Jobs = jobs.New(r.ctx, func(v jobs.Snapshot) {
		result, _ := json.Marshal(v)
		args, _ := json.Marshal(map[string]string{"command": v.Label})
		if v.Kind != "shell" {
			args, _ = json.Marshal(map[string]string{"label": v.Label})
		}
		md := render.Tool(v.Kind, args, result)
		if v.Kind == "btw" {
			md.Detail = r.btwDetail(v)
		} else {
			md.Detail = r.JobDetail(v.ID, md.Detail)
		}
		r.orderMu.Lock()
		sessionID := r.Current()
		entry, e := r.Store.Append(sessionID, "", v.Owner, "status", "", false, map[string]any{"type": "job_completion", "job": v, "markdown": md})
		if e == nil && v.WakeOnExit && r.ctx.Err() == nil {
			e = r.queueNotificationLocked(fmt.Sprintf(`{"type":"job_exit","job_id":%q,"status":%q}`, v.ID, v.Status))
		}
		r.orderMu.Unlock()
		if e != nil {
			r.emit(Event{Kind: "status", Text: "Job completion history failed: " + e.Error(), SessionID: sessionID})
			return
		}
		r.emit(Event{Kind: "job", Text: md.Summary, EntryID: entry, SessionID: sessionID, Actor: v.Owner})
		if v.Kind == "btw" && v.Status != "cancelled" {
			r.emit(Event{Kind: "btw_result", Text: md.Detail, EntryID: entry, SessionID: sessionID})
		}
	})
	r.Jobs.OnState = r.publishJob
	tool.AddFiles(r.Tools, r.Workspace)
	tool.AddShell(r.Tools, r.Jobs, r.Workspace, r)
	tool.AddWeb(r.Tools, httpClient(), r.searchConfig)
	tool.AddSkills(r.Tools, r.Skills)
	w := newWakeups(r.ctx, r.publishTimer)
	r.mu.Lock()
	r.timers = w
	r.mu.Unlock()
	r.addWakeupTools()
	r.addImageTool()
	r.questions.mu.Lock()
	r.questions.pending = nil
	r.questions.mu.Unlock()
	r.addQuestionTool()
	r.addSubagentTool()
}

// Current returns the conversation pointer used by completion writers.
func (r *Runtime) Current() string { r.mu.Lock(); defer r.mu.Unlock(); return r.current }

// CurrentSession returns copied metadata, including a blank conversation that
// has no database row yet. Blank identities are stable until an explicit switch.
func (r *Runtime) CurrentSession() (history.Session, error) {
	r.mu.Lock()
	id, persisted, selection := r.current, r.persisted, r.selection
	r.mu.Unlock()
	if !persisted {
		return history.Session{ID: id, LineageID: id, Name: "New session", Model: selection}, nil
	}
	return r.Store.Session(id)
}

// Entries returns current selected history; a blank conversation has no entries.
func (r *Runtime) Entries() ([]history.Entry, error) {
	r.mu.Lock()
	id, persisted := r.current, r.persisted
	r.mu.Unlock()
	if !persisted {
		return nil, nil
	}
	return r.Store.Branch(id, 0)
}

// Generation identifies live-state ownership across explicit session changes.
// Compaction preserves it so pending interactions remain usable.
func (r *Runtime) Generation() uint64 { r.mu.Lock(); defer r.mu.Unlock(); return r.generation }
func (r *Runtime) emit(e Event) {
	r.mu.Lock()
	if e.SessionID == "" {
		e.SessionID = r.current
	}
	e.Generation = r.generation
	r.mu.Unlock()
	if r.Emit != nil {
		r.Emit(e)
	}
}

// Interrupt cancels foreground request/tools, leaving independent jobs until runtime exit.
func (r *Runtime) Interrupt() {
	r.mu.Lock()
	if r.activeCancel != nil {
		r.activeCancel()
	}
	r.mu.Unlock()
}

// Close stops the runtime and joins background naming, process groups and timers.
// Call it after Run and Command return; Interrupt cancels active foreground work.
func (r *Runtime) Close() {
	if r.retentionStop != nil {
		r.retentionStop()
		r.retentionStop = nil
	}
	r.cancel()
	r.Interrupt()
	r.stopNaming()
	r.Jobs.Close()
	r.timers.close()
	r.clearImages()
}

// HasNotifications reports queued runtime messages without treating history as live state.
func (r *Runtime) HasNotifications() bool {
	r.orderMu.Lock()
	defer r.orderMu.Unlock()
	return len(r.notifications) > 0 || len(r.steers) > 0
}

// Run executes one user or notification turn through complete tool/result cycles.
func (r *Runtime) Run(message *provider.Message) (err error) {
	return r.run(message, nil)
}

func (r *Runtime) run(message *provider.Message, input *InputAdmission) (err error) {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	if e := r.checkContext(); e != nil {
		return e
	}
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	r.mu.Lock()
	r.activeCancel = cancel
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.activeCancel = nil; r.activeTurn = ""; r.mu.Unlock() }()
	trigger := "async"
	if message != nil || input != nil {
		trigger = "user"
	}
	if event, e := r.ApplyModel(""); e != nil {
		return e
	} else if event.Kind != "" {
		r.emit(event)
	}
	selection := r.CurrentSelection()
	if e := ctx.Err(); e != nil {
		return e
	}
	r.mu.Lock()
	id, persisted := r.current, r.persisted
	r.mu.Unlock()
	var turn string
	var firstEntry int64
	var e error
	e = r.Workspace.Admit(ctx, func() error {
		if input != nil {
			input.mu.Lock()
			defer input.mu.Unlock()
			if !input.pending || input.generation != r.Generation() {
				return errInputCancelled
			}
			m := input.input.Message()
			message = &m
			// Hold the ticket through the commit: a losing cancellation must
			// not interrupt this turn or restore an already admitted input.
			defer input.clearLocked()
		}
		if persisted {
			turn, firstEntry, e = r.Store.AdmitTurn(id, trigger, selection, message)
			return e
		}
		if message == nil {
			return errors.New("session is empty; send a user message before running background notifications")
		}
		turn, firstEntry, e = r.Store.StartSession(id, r.Workspace.Root, selection, *message)
		if e != nil {
			return fmt.Errorf("save first message: %w", e)
		}
		if e == nil {
			r.mu.Lock()
			r.persisted = true
			r.mu.Unlock()
		}
		return e
	})
	if e != nil {
		return e
	}
	r.mu.Lock()
	r.activeTurn = turn
	r.mu.Unlock()
	status := "failed"
	started := time.Now()
	modelTime := time.Duration(0)
	outputTokens := 0
	haveUsage := true
	namingChecked := false
	defer func() {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			status = "interrupted"
		}
		if e := r.Store.FinishTurn(turn, status); err == nil && e != nil {
			err = e
		}
		avg := "N/A"
		if haveUsage && modelTime > 0 {
			avg = fmt.Sprintf("%.1f tok/s", float64(outputTokens)/modelTime.Seconds())
		}
		elapsed := time.Since(started)
		displayStatus := status
		if status == "completed" {
			displayStatus = "complete"
		}
		text := fmt.Sprintf("Turn %s · %s · avg %s", displayStatus, turnDuration(elapsed), avg)
		result := map[string]any{"type": "turn_end", "status": status, "text": text, "wall_ms": elapsed.Milliseconds()}
		if err != nil {
			result["error"] = err.Error()
		}
		id, e := r.Store.Append(r.Current(), turn, "main", "status", "", false, result)
		if err == nil && e != nil {
			err = e
		}
		r.emit(Event{Kind: "status", Text: text, EntryID: id})

	}()
	if message != nil {
		entry := firstEntry
		if entry == 0 {
			entry, e = r.Store.Append(r.Current(), turn, "main", "message", "user", true, *message)
			if e != nil {
				return e
			}
		}
		r.emit(Event{Kind: "message", Text: message.DisplayText(), EntryID: entry, Human: true})
	}
	priorAttempts := 0
	var recovery *provider.Message
	for {
		if event, err := r.ApplyModel(turn); err != nil {
			return err
		} else if event.Kind != "" {
			r.emit(event)
		}
		selection = r.CurrentSelection()
		if e = ctx.Err(); e != nil {
			return e
		}
		system := systemTemplate
		admitted, contextMessage, e := r.admitMain(ctx, turn, selection)
		if errors.Is(e, errNeedsCompaction) {
			r.emit(Event{Kind: "status", Text: "Compacting context…"})
			result, err := r.compactContext(ctx, "", selection, recovery)
			if err != nil {
				return fmt.Errorf("automatic compaction failed: %w", err)
			}
			r.emit(Event{Kind: "continuation", Text: result})
			continue
		}
		if e != nil {
			return e
		}
		request, messages := admitted.RequestID, admitted.Messages
		defs := r.Tools.Definitions()
		if admitted.ContextEntry != 0 {
			r.emit(Event{Kind: "runtime_context", Text: contextLabel(*contextMessage), EntryID: admitted.ContextEntry})
		}
		for _, entry := range admitted.SteerEntries {
			v, e := r.Store.Entry(entry)
			if e != nil {
				return e
			}
			var m provider.Message
			if e = json.Unmarshal(v.Content, &m); e != nil {
				return e
			}
			r.emit(Event{Kind: "message", Text: m.DisplayText(), EntryID: entry, Human: true})
		}
		for _, entry := range admitted.NoticeEntries {
			v, e := r.Store.Entry(entry)
			if e != nil {
				return e
			}
			var m provider.Message
			if e = json.Unmarshal(v.Content, &m); e != nil {
				return e
			}
			var wake struct {
				ID    string `json:"wakeup_id"`
				Fired int    `json:"fired_count"`
			}
			if json.Unmarshal([]byte(m.Content), &wake) == nil && wake.ID != "" {
				r.timers.deliveredThrough(wake.ID, wake.Fired)
			}
			r.emit(Event{Kind: "message", Text: m.Content, EntryID: entry})
		}
		promptEntry, e := r.Store.RecordSystemPrompt(r.Current(), turn, "main", request, system)
		if e != nil {
			return e
		}
		r.emit(Event{Kind: "system_prompt", Text: "System prompt · inspect", EntryID: promptEntry})
		reply := provider.Message{Role: "assistant"}
		var text strings.Builder
		var usage *provider.Usage
		responseID := ""
		serviceTier := ""
		requestStart := time.Now()
		r.mu.Lock()
		r.mainPrefix = append([]provider.Message(nil), messages...)
		r.prefixSelection, r.prefixTurn = selection, turn
		r.mu.Unlock()
		callbackFailed := false
		streamErr := r.Provider.Stream(ctx, provider.Request{ConversationID: r.Current(), Selection: selection, System: system, Messages: messages, Tools: defs, OutputTokens: selection.Model.Budget.OutputAllowance, PriorAttempts: priorAttempts}, func(event provider.StreamEvent) (err error) {
			defer func() { callbackFailed = callbackFailed || err != nil }()
			if e := ctx.Err(); e != nil {
				return e
			}
			switch event.Kind {
			case "retry":
				return r.retryNotice(turn, "main", request, "coding", event.Retry)
			case "call_start":
				return r.toolAnnouncement(turn, "main", request, event.CallStart)
			case "call_progress":
				return r.toolProgress("main", request, event.CallProgress)
			case "text":
				text.WriteString(event.Text)
				r.emit(Event{Kind: "delta", Text: render.Clean(event.Text), RequestID: request})
			case "call":
				if event.Call == nil {
					return errors.New("provider emitted nil call")
				}
				reply.Calls = append(reply.Calls, *event.Call)
			case "phase":
				reply.Phase = event.Phase
			case "state":
				return reply.AppendState(selection, event.StateVersion, event.StateItem)
			case "completed":
				usage = event.Usage
				responseID = event.ResponseID
				serviceTier = event.ServiceTier
			}
			return nil
		})
		reply.Content = text.String()
		r.recordUsage(usage)
		if streamErr != nil {
			// State/call callbacks can be interrupted halfway through completion.
			// Canonical partial output remains inspectable, but cannot replay a
			// native payload whose corresponding calls were never committed.
			reply.State = nil
		}
		r.emit(Event{Kind: "tool_stream_end", PendingKey: pendingToolKey(request, ""), Text: streamState(streamErr)})
		elapsed := time.Since(requestStart)
		modelTime += elapsed
		if usage == nil {
			haveUsage = false
		} else {
			outputTokens += usage.OutputTokens
		}
		requestStatus := "completed"
		if streamErr != nil {
			requestStatus = "failed"
		}
		attempt := map[string]any{"duration_ms": elapsed.Milliseconds(), "response_id": responseID, "service_tier": serviceTier, "usage": usage, "status": requestStatus}
		if streamErr != nil {
			attempt["error"] = streamErr.Error()
		}
		if e = r.Store.FinishRequest(request, requestStatus, []any{attempt}); e != nil {
			return e
		}
		if streamErr == nil {
			r.mu.Lock()
			r.reported = nil
			if usage != nil {
				r.reported = copyReported(&ReportedUsage{Model: selection.Model.ID + " · " + selection.Variant, RequestID: request, Tokens: *usage})
			}
			r.mu.Unlock()
		}
		r.emit(Event{Kind: "usage"})
		var callIDs []string
		if reply.Content != "" || len(reply.Calls) > 0 || reply.State != nil {
			id, ids, e := r.Store.Assistant(r.Current(), turn, "main", request, reply)
			if e != nil {
				return e
			}
			callIDs = ids
			r.emit(Event{Kind: "assistant", Text: reply.Content, EntryID: id, RequestID: request})
		}
		if _, e = r.runToolBatch(ctx, turn, "main", r.Tools, reply.Calls, callIDs, streamErr); e != nil {
			return e
		}
		r.emit(Event{Kind: "usage"})
		if streamErr != nil {
			var partial *provider.PartialError
			if callbackFailed || !errors.As(streamErr, &partial) {
				return streamErr
			}
			message, err := r.recoverPartial(ctx, turn, "main", request, priorAttempts, partial)
			if err != nil {
				return err
			}
			recovery = &message
			priorAttempts = partial.Retry.Attempt - 1
			continue
		}
		priorAttempts, recovery = 0, nil
		if !namingChecked && message != nil && r.AutoName {
			namingChecked = true
			r.startNaming(turn, selection, *message, reply)
		}
		pending := r.HasNotifications()
		if len(reply.Calls) == 0 && !pending {
			status = "completed"
			return nil
		}
	}
}

// turnDuration formats elapsed time in whole seconds, omitting zero units.
func turnDuration(elapsed time.Duration) string {
	seconds := max(int64(0), int64(elapsed/time.Second))
	var text strings.Builder
	for _, unit := range []struct {
		seconds int64
		suffix  string
	}{{86400, "d"}, {3600, "h"}, {60, "min"}, {1, "s"}} {
		if count := seconds / unit.seconds; count > 0 {
			text.WriteString(strconv.FormatInt(count, 10))
			text.WriteString(unit.suffix)
			seconds %= unit.seconds
		}
	}
	if text.Len() == 0 {
		return "0s"
	}
	return text.String()
}

// Command returns help during any turn; other commands serialize with history
// and lifecycle operations. Compaction requests a model summary.
func (r *Runtime) Command(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "/help" {
		return helpMarkdown, nil
	}
	r.runMu.Lock()
	defer r.runMu.Unlock()
	parts := strings.SplitN(text, " ", 2)
	switch parts[0] {
	case "/new", "/clear", "/load", "/undo", "/redo", "/branch":
		if r.retentionStop != nil {
			r.retentionStop()
			r.retentionStop = nil
		}
		defer func() { r.retentionStop = r.startRetention() }()
	}
	switch parts[0] {
	case "/undo", "/redo", "/branch", "/compact":
		if err := r.checkContext(); err != nil {
			return "", err
		}
	}
	arg := ""
	if len(parts) == 2 {
		arg = strings.TrimSpace(parts[1])
	}
	r.mu.Lock()
	persisted := r.persisted
	r.mu.Unlock()
	if !persisted {
		switch parts[0] {
		case "/undo", "/redo", "/branch", "/compact", "/export", "/rename":
			return "", errors.New("session is empty; send a message first")
		}
	}
	switch parts[0] {
	case "/new", "/clear":
		r.stopNaming()
		r.Jobs.Close()
		r.timers.close()
		r.clearImages()
		r.mu.Lock()
		r.current = history.NewID("session")
		r.persisted = false
		if parts[0] == "/new" {
			// Workers have joined, so no response from the old run can arrive
			// after its counters are cleared. Other session changes keep totals.
			r.totals = UsageTotals{}
		}
		r.mu.Unlock()
		r.resetTransient()
		return "New session · " + r.Current(), nil
	case "/load":
		v, e := r.Store.Session(arg)
		if e != nil {
			return "", e
		}
		if v.WorkspaceID != "" {
			var path string
			if e = r.Store.DB.QueryRow("SELECT path FROM workspaces WHERE id=?", v.WorkspaceID).Scan(&path); e != nil {
				return "", e
			}
			if path != r.Workspace.Root {
				return "", errors.New("session belongs to another workspace")
			}
		}
		// Prepare an independent snapshot before canceling the current runtime.
		v, e = r.Store.Load(arg)
		if e != nil {
			return "", e
		}
		notices, e := r.Store.PendingRecoveryNotifications(r.ctx, v.ID)
		if e != nil {
			return "", fmt.Errorf("read recovered notifications: %w", e)
		}
		selection := r.CurrentSelection()
		var switchEntry int64
		switchText := ""
		if v.ReadOnly {
			selection = v.Model
		} else if v.Model.Provider != selection.Provider || v.Model.Model.ID != selection.Model.ID || v.Model.Variant != selection.Variant {
			switchText = fmt.Sprintf("Model switched · %s · %s", selection.Model.ID, selection.Variant)
			switchEntry, e = r.Store.SwitchModel(v.ID, "", v.Model, selection, switchText)
			if e != nil {
				return "", e
			}
		}
		r.stopNaming()
		r.Jobs.Close()
		r.timers.close()
		r.clearImages()
		r.mu.Lock()
		r.current = v.ID
		r.persisted = true
		r.selection = selection
		r.mu.Unlock()
		r.resetTransient()
		r.orderMu.Lock()
		r.notifications = notices
		r.orderMu.Unlock()

		if switchEntry != 0 {
			r.emit(Event{Kind: "status", Text: switchText, EntryID: switchEntry, SessionID: v.ID})
		}
		return "Loaded · " + v.Name, nil
	case "/undo", "/redo":
		r.stopNaming()
		r.Jobs.Close()
		r.timers.close()
		r.clearImages()
		r.resetTransient()
		var target history.RestoreTarget
		var e error
		if parts[0] == "/undo" {
			target, e = r.Store.UndoTarget(r.Current())
		} else {
			v, err := r.Store.Session(r.Current())
			if err != nil {
				return "", err
			}
			if v.RedoTip == 0 {
				return "", errors.New("nothing to redo")
			}
			target, e = r.Store.BranchTarget(r.Current(), v.RedoTip)
		}
		if e != nil {
			return "", e
		}
		e = r.Workspace.Restore(r.ctx, r.Current(), target)
		return strings.TrimPrefix(parts[0], "/") + " completed", e
	case "/branch":
		id, err := strconv.ParseInt(arg, 10, 64)
		if err != nil || id < 0 {
			return "", errors.New("usage: /branch <entry-ID>")
		}
		return "", r.RestoreBranch(id)
	case "/export":
		if arg == "" {
			return "", errors.New("usage: /export <new-path>")
		}
		return "Exported · " + arg, r.Store.Export(r.Current(), arg)
	case "/rename":
		if arg == "" {
			return "", errors.New("usage: /rename <title>")
		}
		sessionID := r.Current()
		if err := r.Store.RenameSession(sessionID, arg); err != nil {
			return "", err
		}
		r.emit(Event{Kind: "session_name", Text: arg, SessionID: sessionID})
		return "", nil
	case "/sessions":
		list, e := r.Store.Sessions(r.Workspace.Root)
		if e != nil {
			return "", e
		}
		var b strings.Builder
		for _, v := range list {
			fmt.Fprintf(&b, "%s · %s · read_only=%t\n", v.ID, v.Name, v.ReadOnly)
		}
		return b.String(), nil
	case "/history":
		tree, err := r.History()
		if err != nil {
			return "", err
		}
		b, err := json.MarshalIndent(tree, "", "  ")
		return string(b), err
	case "/jobs":
		b, _ := json.MarshalIndent(r.Jobs.List("main", true), "", "  ")
		return string(b), nil
	case "/timers":
		b, _ := json.MarshalIndent(r.timers.list(), "", "  ")
		return string(b), nil
	case "/compact":
		return r.compact(arg)
	default:
		return "", fmt.Errorf("unknown command %s", parts[0])
	}
}
