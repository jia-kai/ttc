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
	"ttc/internal/llm"
	"ttc/internal/messagestream"
	"ttc/internal/prompts"
	"ttc/internal/render"
	"ttc/internal/skills"
	"ttc/internal/tool"
	"ttc/internal/workspace"
)

// Event contains a view update, never authentication state. Inspection actions
// belong to stream items; EntryID also identifies direct execution records.
type Event struct {
	Generation  uint64 // Live-state owner; compaction preserves it, explicit resets advance it.
	Kind, Text  string
	SessionID   string // Runtime conversation at emission; the frontend discards stale-view events.
	EntryID     int64
	RequestID   int64               // Producing model request for streamed/completed assistant text.
	Actor       string              // Producing actor for any event; empty means the main actor.
	CallID      string              // Stable tool-card identity across transient updates and the final record.
	PendingKey  string              // Request-scoped streamed announcement to replace when intent is committed.
	JobID       string              // Optional live job for bounded inspector polling; never revived from history.
	Detail      string              // Bounded detail for a transient tool update; final detail is loaded from history.
	MessageType llm.MessageType     // Canonical semantics for message events; zero for transient events.
	InputSource string              // Human input source (normal, queue or steer); empty for non-input events.
	InputTimeMS int64               // Original human-input commit time in Unix milliseconds; zero for non-input events.
	Image       *ImageSnapshot      // Immutable image_show snapshot, not a live interaction handle.
	Question    *QuestionForm       // Snapshot for question lifecycle events; never persisted as a live handle.
	Retry       *llm.Retry          // Foreground retry backoff metadata; nil for background session naming.
	StreamItem  *messagestream.Item // Provider-boundary presentation item; never model context.
}

// Inference is the protocol dependency consumed by sessions. Catalog discovery,
// authorization, credentials and cache lifecycle are supplied to their own owners,
// not required of every inference backend. Callback errors terminate a stream;
// only a backend PartialError authorizes an application continuation request.
type Inference interface {
	Stream(context.Context, llm.Request, func(llm.StreamEvent) error) error
	// Cancel is attempt-scoped best-effort remote cancellation by response ID.
	// Stream owns cleanup with a fresh bounded context, independently of local
	// transport abort. Missing identity/unsupported APIs must no-op, never cancel
	// another attempt; implementations must honor context and concurrency.
	Cancel(context.Context, string) error
	EstimateReplay(llm.Message) int
	TokenizerID(llm.ModelSpec) string
}

// Runtime owns exactly one main session, and joins transient work before switching it.
// Run, RunInput, and Command are called serially by the frontend;
// Interrupt, CancelForeground and RequestModel are concurrent-safe.
type Runtime struct {
	Store             *history.Store
	Workspace         *workspace.Manager
	Provider          Inference
	selection         llm.Selection
	pendingModel      *llm.Selection
	Skills            *skills.Catalog
	Tools             *tool.Registry
	defaultTools      []llm.ToolDefinition // Owned immutable default schemas for this runtime config; never aliases Tools.
	searchConfig      tool.WebSearchConfig // Operator settings, excluded from model context/history.
	Jobs              *jobs.Manager
	Emit              func(Event)
	ctx               context.Context
	cancel            context.CancelFunc
	mu                sync.Mutex
	runMu             sync.Mutex // Serializes main foreground turns and idle lifecycle commands.
	orderMu           sync.Mutex // Serializes event publication and immutable request admission.
	boundaryMu        sync.Mutex // Orders boundary batches; never acquired by frontend/runtime state reads.
	fatalCompaction   string     // Main context failure, including a failed invalidation write.
	orderError        error      // Persistence failure prevents inference on an incomplete live snapshot.
	publishedJobs     map[string]jobs.Snapshot
	publishedTimers   map[string]wakeup
	publishedChildren map[string]tool.ChildView
	actorAssignments  map[string]*childAssignment // orderMu; assignment-specific event delivery.
	actorSignals      map[string]chan struct{}    // orderMu; event-driven inference resumption.
	awaiting          map[string]bool             // orderMu; transient UI state, never restored.
	awaitRequests     map[string]awaitRequest     // orderMu; consumed only after tool settlement.
	activityStop      func()
	children          map[string]*codingChild // Owned by childStartMu; live handles never restored.
	childStartMu      sync.Mutex              // Serializes child capacity checks and admission, never child execution.
	routeMu           sync.RWMutex            // Keeps child commits/file mutations within the current continuation.
	current           string
	persisted         bool   // False until the first user turn/message is committed atomically.
	generation        uint64 // Advances on explicit transient resets, never compaction.
	activeCancel      context.CancelCauseFunc
	notifications     []llm.Message
	steers            []contextbuild.Input // Original transient human inputs, admitted only at a model boundary.
	activeTurn        string               // Main inference turn, distinct from steering undo checkpoints.
	AutoName          bool
	timers            *wakeups
	questions         questions
	images            imageInteractions
	usage             ContextUsage
	estimator         *contextbuild.Estimator // Owns main-request input estimates and provider feedback.
	reported          *ReportedUsage          // Protected by mu; loads restore selected parent feedback after reset.
	totals            UsageTotals             // All inference usage since construction or /new, protected by mu.
	mainContext       contextCursor           // Owned by serial Run/Command; reset on explicit session changes.
	mainPrefix        []llm.Message           // Latest balanced main request input, immutable after publication under mu.
	prefixSelection   llm.Selection
	prefixTurn        string
	prefixSession     string // Durable source of mainPrefix, frozen at request admission.
	prefixTip         int64  // Selected branch tip of prefixSession, not a global event cutoff.
	namingCtx         context.Context
	namingCancel      context.CancelFunc
	namingDone        chan struct{} // Serial main-loop ownership; closed after the one naming task finishes.
}

// New constructs an active runtime. An empty session ID starts an in-memory
// blank conversation; a nonempty ID must refer to an already persisted session.
// Loaded compaction-boundary notifications are queued before any work starts.
func New(ctx context.Context, store *history.Store, w *workspace.Manager, p Inference, selection llm.Selection, session string, catalog *skills.Catalog, search tool.WebSearchConfig, emit func(Event)) (*Runtime, error) {
	ctx, cancel := context.WithCancel(ctx)
	r := &Runtime{Store: store, Workspace: w, Provider: p, selection: selection, Skills: catalog, searchConfig: search, Emit: emit, ctx: ctx, cancel: cancel, current: history.NewID("session"), AutoName: true}
	r.replaceAccounting(preparedAccounting{})
	r.resetLiveWork()
	if session != "" {
		target, err := r.prepareSession(session, false)
		if err != nil {
			r.Close()
			return nil, err
		}
		r.installSession(target)
	}
	r.activityStop = r.startActivityRefresh()
	return r, nil
}

func (r *Runtime) resetLiveWork() {
	r.resetChildren()
	r.orderMu.Lock()
	r.publishedJobs = map[string]jobs.Snapshot{}
	r.publishedTimers = map[string]wakeup{}
	r.publishedChildren = map[string]tool.ChildView{}
	r.actorAssignments = map[string]*childAssignment{}
	r.actorSignals = map[string]chan struct{}{}
	r.awaiting = map[string]bool{}
	r.awaitRequests = map[string]awaitRequest{}
	r.orderError = nil
	r.notifications = nil
	r.steers = nil
	r.orderMu.Unlock()
	r.mu.Lock()
	r.fatalCompaction = ""
	r.mainContext = contextCursor{}
	r.mainPrefix = nil
	r.prefixTurn = ""
	r.prefixSession, r.prefixTip = "", 0
	r.generation++
	r.mu.Unlock()
	r.namingCtx, r.namingCancel = context.WithCancel(r.ctx)
	r.namingDone = nil
	r.Tools = tool.NewRegistry()
	r.Jobs = jobs.New(r.ctx, func(v jobs.Snapshot) {
		result, _ := json.Marshal(v)
		// Summary identity is independent of inspection customization.
		args, _ := json.Marshal(map[string]string{"label": v.Label})
		md := render.Tool(v.Kind, args, result)
		md.Detail = r.JobDetail(v.ID, "")
		r.orderMu.Lock()
		sessionID := r.Current()
		entry, e := r.Store.Append(sessionID, "", v.Owner, "status", "", false, map[string]any{"type": "job_completion", "job": v, "markdown": md})
		// Child assignments use the canonical answer/failure handoff rather than
		// duplicate generic job notifications. Diagnostics remain opt-in.
		if e == nil && v.WakeOnExit && v.Kind != "subagent" && r.ctx.Err() == nil {
			e = r.queueActorNotificationLocked(v.Owner, jobExitContent(v))
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
	w := newWakeups(r.ctx, r.publishTimer)
	w.release = r.Jobs.ReleaseOwned
	r.mu.Lock()
	r.timers = w
	r.mu.Unlock()
	r.questions.mu.Lock()
	r.questions.pending = nil
	r.questions.mu.Unlock()
	tool.AddFiles(r.Tools, r.Workspace)
	tool.AddShell(r.Tools, r.Jobs, r.Workspace, r)
	tool.AddWatch(r.Tools, r.Jobs, r.Workspace)
	tool.AddTmuxWatchRun(r.Tools, r.Jobs, r.Workspace)
	tool.AddWeb(r.Tools, httpClient(), r.searchConfig)
	tool.AddSkills(r.Tools, r.Skills)
	r.addWakeupTools()
	r.addAwaitTool()
	r.addImageTool()
	r.addQuestionTool()
	r.addSubagentTool()
	r.addConversationExportTool()
	// Definitions copies values, but Parameters still aliases registry storage.
	// Freeze defaults only after constructing real handlers against fresh live
	// managers. Preparation reuses this snapshot, not a customized active registry.
	r.defaultTools = r.Tools.Definitions()
	for i := range r.defaultTools {
		r.defaultTools[i].Parameters = append(json.RawMessage(nil), r.defaultTools[i].Parameters...)
	}
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

// Interrupt cancels foreground request/tools, leaving independent jobs until runtime
// exit. An admitted main turn that stops records a developer interruption notice
// after its tool results settle; calling Interrupt while idle records nothing.
func (r *Runtime) Interrupt() {
	r.cancelForeground(errUserInterrupted)
}

// CancelForeground cancels foreground work for cleanup without attributing the
// cancellation to the user. Independent jobs remain alive until runtime exit.
func (r *Runtime) CancelForeground() {
	r.cancelForeground(context.Canceled)
}

func (r *Runtime) cancelForeground(cause error) {
	r.mu.Lock()
	if r.activeCancel != nil {
		r.activeCancel(cause)
	}
	r.mu.Unlock()
}

// Close stops the runtime and joins background naming, process groups and timers.
// Call it after Run and Command return; Interrupt cancels active foreground work.
func (r *Runtime) Close() {
	if r.activityStop != nil {
		r.activityStop()
		r.activityStop = nil
	}
	r.cancel()
	r.CancelForeground()
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
func (r *Runtime) Run(message *llm.Message) (err error) {
	return r.run(message, nil)
}

// errUserInterrupted distinguishes an explicit foreground interruption from
// provider failures and cancellation inherited from the runtime's parent.
var errUserInterrupted = errors.New("user interrupted turn")

func (r *Runtime) run(message *llm.Message, input *InputAdmission) (err error) {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	if e := r.checkContext(false); e != nil {
		return e
	}
	ctx, cancel := context.WithCancelCause(r.ctx)
	defer cancel(nil)
	defer func() {
		r.orderMu.Lock()
		delete(r.awaitRequests, "main")
		r.orderMu.Unlock()
	}()
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
		if err != nil && context.Cause(ctx) == errUserInterrupted {
			// Run and Command are serialized. Commit after tool settlement so
			// the next input or wakeup follows a balanced exchange and this notice.
			message := llm.Message{Type: llm.TypeRuntimeInstruction, Content: prompts.SessionUserInterrupted}
			id, e := r.Store.Append(r.Current(), turn, "main", "message", message.Role(), true, message)
			if e != nil {
				failure := fmt.Errorf("record user interruption: %w", e)
				r.orderMu.Lock()
				r.orderError = errors.Join(r.orderError, failure)
				r.orderMu.Unlock()
				err = errors.Join(err, failure)
			} else {
				r.emit(Event{Kind: "message", Text: message.DisplayText(), EntryID: id, MessageType: message.Type})
			}
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
		event := Event{Kind: "message", Text: message.DisplayText(), EntryID: entry, MessageType: message.Type}
		if message.Type == llm.TypeUserInput {
			committed, err := r.Store.Entry(entry)
			if err != nil {
				return err
			}
			event.InputSource = message.InputSource
			if event.InputSource == "" {
				event.InputSource = "normal"
			}
			event.InputTimeMS = committed.CreatedMS
		}
		r.emit(event)
	}
	priorAttempts := 0
	var recovery *llm.Message
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
		admitted, _, e := r.admitMain(ctx, turn, selection)
		if errors.Is(e, errNeedsCompaction) {
			r.emit(Event{Kind: "status", Text: "Compacting context…"})
			result, err := r.compactContext(ctx, "", selection, recovery, false)
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
		for _, entry := range admitted.SteerEntries {
			v, e := r.Store.Entry(entry)
			if e != nil {
				return e
			}
			var m llm.Message
			if e = json.Unmarshal(v.Content, &m); e != nil {
				return e
			}
			r.emit(Event{Kind: "message", Text: m.DisplayText(), EntryID: entry, MessageType: m.Type, InputSource: "steer", InputTimeMS: v.CreatedMS})
		}
		if e = r.deliverNoticeEntries("main", admitted.NoticeEntries); e != nil {
			return e
		}
		_, e = r.Store.RecordSystemPrompt(r.Current(), turn, "main", request, system)
		if e != nil {
			return e
		}
		reply := llm.Message{Type: llm.TypeAssistant}
		var text strings.Builder
		var usage *llm.Usage
		responseID := ""
		serviceTier := ""
		requestStart := time.Now()
		r.mu.Lock()
		r.mainPrefix = append([]llm.Message(nil), messages...)
		r.prefixSelection, r.prefixTurn = selection, turn
		r.prefixSession, r.prefixTip = r.current, admitted.EntryTip
		r.mu.Unlock()
		projected, e := contextbuild.ProjectRetainedInputs(messages)
		if e != nil {
			return e
		}
		callbackFailed := false
		streamErr := r.stream(ctx, turn, "main", request, llm.Request{ConversationID: r.Current(), Selection: selection, System: system, Messages: projected, Tools: defs, OutputTokens: selection.Model.Budget.OutputAllowance, PriorAttempts: priorAttempts}, func(event llm.StreamEvent) (err error) {
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
		if !usage.Usable() {
			haveUsage = false
		} else {
			outputTokens += usage.OutputTokens
		}
		requestStatus := "completed"
		if streamErr != nil {
			requestStatus = "failed"
		}
		attempt := history.RequestAttempt{DurationMS: elapsed.Milliseconds(), ResponseID: responseID, ServiceTier: serviceTier, Usage: usage, Status: requestStatus}
		if streamErr != nil {
			attempt.Error = streamErr.Error()
		}
		if e = r.Store.FinishRequest(request, history.RequestCompletion{Status: requestStatus, Attempts: []history.RequestAttempt{attempt}}); e != nil {
			return e
		}
		if streamErr == nil {
			if usage.Usable() {
				if result, ok := r.estimator.ObserveUsage(request, usage.InputTokens); ok {
					r.mu.Lock()
					r.usage = usageFromEstimate(selection, result)
					r.mu.Unlock()
				}
			}
			r.mu.Lock()
			r.reported = nil
			if usage.Usable() {
				r.reported = copyReported(&ReportedUsage{Model: selection.Model.ID + " · " + selection.Variant, Tokens: *usage})
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
		if e = r.settleAwait(ctx, "main"); e != nil {
			return e
		}
		r.emit(Event{Kind: "usage"})
		if streamErr != nil {
			var partial *llm.PartialError
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
		defer func() { r.refreshLoadedActivity(r.ctx, time.Now()) }()
	}
	switch parts[0] {
	case "/undo", "/redo", "/branch", "/compact", "/force-compact":
		if err := r.checkContext(true); err != nil {
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
		case "/undo", "/redo", "/branch", "/compact", "/force-compact", "/export", "/rename":
			return "", errors.New("session is empty; send a message first")
		}
	}
	switch parts[0] {
	case "/new", "/clear":
		r.stopLiveWork()
		r.mu.Lock()
		r.current = history.NewID("session")
		r.persisted = false
		if parts[0] == "/new" {
			// Workers have joined, so no response from the old run can arrive
			// after its counters are cleared. Other session changes keep totals.
			r.totals = UsageTotals{}
		}
		r.mu.Unlock()
		r.resetLiveWork()
		r.replaceAccounting(preparedAccounting{})
		return "New session · " + r.Current(), nil
	case "/load":
		v, err := r.load(arg)
		if err != nil {
			return "", err
		}
		return "Loaded · " + v.Name, nil
	case "/undo", "/redo":
		r.stopLiveWork()
		r.resetLiveWork()
		r.replaceAccounting(preparedAccounting{})
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
	case "/compact", "/force-compact":
		return r.compact(arg, parts[0] == "/force-compact")
	default:
		return "", fmt.Errorf("unknown command %s", parts[0])
	}
}
