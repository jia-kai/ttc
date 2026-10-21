package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	contextbuild "ttc/internal/context"
	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/prompts"
	"ttc/internal/render"
	"ttc/internal/tool"
)

// codingChild retains one isolated context. childStartMu protects its fields;
// assignment's admission queue has its own orderMu discipline. Only the running
// worker owns messages/cursor until publishing an idle state.
type codingChild struct {
	id, label, state, turn, job string
	selection                   llm.Selection
	tools                       *tool.Registry
	messages                    []llm.Message
	cursor                      contextCursor
	closing                     bool
	assignment                  *childAssignment
	lastAnswer                  string // Last nonempty assistant text across persistent assignments, bounded like parent delivery.
	lastResult                  int64
	lastTruncated               bool
}

type childAssignment struct {
	// orderMu protects the admission queue and terminal admission gate.
	accepting       bool
	queued          []llm.Message
	runtimeNotices  []*pendingRuntimeNotice          // Durable source refs; bounded batches materialize only at admission.
	runtimeTimers   map[string]*pendingRuntimeNotice // O(1) coalescing of pending timer firings.
	queuedTokens    int                              // Conservative wire-token estimate, including event envelopes.
	queueTokenLimit int                              // Bounded by the model's reserved next-turn input headroom.
	turn, job       string
	finish, result  int64 // childStartMu guards updates; each assignment retains distinct output refs.
	answer          string
	truncated       bool
	persistent      bool                // Retain context after this assignment, independent of foreground/background execution.
	background      bool                // Deliver the answer only through its completion notification.
	completion      history.ChildFinish // Terminal handoff metadata, including a local completion-publication failure.
	pending         *llm.Message        // Full emitted output until its assistant entry commits; export on local storage failure.
	inheritedAnswer bool                // No newer assistant text has replaced the previous assignment's last reply.
}

// childAnswer returns a bounded final-answer prefix without retaining the backing
// storage of a large response. Full text stays in its committed entry or failure transcript.
func childAnswer(text string) (string, bool) {
	n := min(len(text), history.MaxChildAnswerBytes)
	if n < len(text) {
		for n > 0 && !utf8.RuneStart(text[n]) {
			n--
		}
	}
	return strings.Clone(text[:n]), n < len(text)
}

func (r *Runtime) resetChildren() {
	r.childStartMu.Lock()
	r.children = map[string]*codingChild{}
	r.childStartMu.Unlock()
}

// view snapshots the context and its latest assignment under childStartMu.
func (child *codingChild) view() tool.ChildView {
	status := "running"
	if child.assignment != nil && child.assignment.completion.Outcome != "" {
		status = child.assignment.completion.Outcome
	}
	return tool.ChildView{ID: child.id, Label: child.label, ChildState: child.state, AssignmentStatus: status, TurnID: child.turn, JobID: child.job}
}

// ChildViews returns current coding contexts, including idle contexts. Children
// cannot inspect sibling contexts; the main actor sees all of them.
func (r *Runtime) ChildViews(actor string) []tool.ChildView {
	r.childStartMu.Lock()
	defer r.childStartMu.Unlock()
	out := []tool.ChildView{}
	for _, child := range r.children {
		if actor == "main" || actor == child.id {
			out = append(out, child.view())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// StopChild closes an idle context or cancels and joins its running assignment.
// Main-only ownership avoids child self-join and cross-actor cancellation.
func (r *Runtime) StopChild(ctx context.Context, actor, id string) (any, error) {
	if actor != "main" {
		return nil, tool.Fail("ownership", prompts.ChildStopOwnership)
	}
	r.childStartMu.Lock()
	child := r.children[id]
	if child == nil {
		r.childStartMu.Unlock()
		return nil, tool.Fail("not_found", prompts.ChildStopUnknown)
	}
	if child.closing {
		r.childStartMu.Unlock()
		return nil, tool.Fail("child_busy", prompts.ChildAlreadyClosing)
	}
	job := child.job
	if child.state == "idle" {
		child.closing = true
		r.childStartMu.Unlock()
		r.Jobs.StopOwned(id)
		r.childStartMu.Lock()
		child.state = "closed"
		view := child.view()
		if err := r.publishChild(view); err != nil {
			r.childStartMu.Unlock()
			return nil, err
		}
		delete(r.children, id)
		r.childStartMu.Unlock()
		return view, nil
	}
	if job == "" {
		r.childStartMu.Unlock()
		return nil, tool.Fail("child_busy", prompts.ChildLaunchPending)
	}
	child.closing = true
	r.childStartMu.Unlock()
	if _, err := r.Jobs.Stop("main", job); err != nil {
		return nil, err
	}
	// Completion may have passed its cleanup decision before closing was set.
	// Join the assignment first so it cannot launch more owned jobs, then sweep.
	r.Jobs.StopOwned(id)
	r.childStartMu.Lock()
	delete(r.children, id)
	view := child.view()
	r.childStartMu.Unlock()
	return view, nil
}

func (r *Runtime) finishChild(child *codingChild, task childTask, err error) error {
	r.orderMu.Lock()
	task.assignment.accepting = false
	for _, message := range task.assignment.queued {
		if message.Type != llm.TypeChildGuidance {
			continue
		}
		body, _ := json.Marshal(map[string]any{"type": "child_message_undelivered", "child_id": child.id, "child_turn_id": task.turn, "job_id": task.assignment.job, "message_event_seq": message.EventSeq})
		if e := r.queueNotificationLocked(string(body)); e != nil {
			err = errors.Join(err, e)
		}
	}
	task.assignment.queued = nil
	task.assignment.queuedTokens = 0
	task.assignment.runtimeNotices = nil
	task.assignment.runtimeTimers = nil
	r.orderMu.Unlock()
	status := "completed"
	if err != nil {
		status = "failed"
	}
	if task.cancelled {
		status = "cancelled"
	}
	// Persistence retains context only, never assignment-owned work. Joining
	// includes callback publication before this context can become idle/reused.
	r.Jobs.StopOwned(child.id)
	r.orderMu.Lock()
	delete(r.actorAssignments, child.id)
	delete(r.awaitRequests, child.id)
	delete(r.actorSignals, child.id)
	r.orderMu.Unlock()
	var transcript string
	var exportErr error
	if status != "completed" {
		r.routeMu.RLock()
		transcript, exportErr = r.Store.ArchiveActorTranscript(r.Current(), child.id, task.assignment.pending)
		r.routeMu.RUnlock()
	}
	r.childStartMu.Lock()
	r.routeMu.RLock()
	r.orderMu.Lock()
	state := "closed"
	if status == "completed" && !child.closing && task.assignment.persistent {
		state = "idle"
	}
	finish := history.ChildFinish{Type: "child_turn_finished", ChildID: child.id, TurnID: task.assignment.turn, JobID: task.assignment.job, Outcome: status, ContextState: state, Persistent: task.assignment.persistent, ResultEntry: task.assignment.result}
	finish.Answer, finish.Truncated = task.assignment.answer, task.assignment.truncated
	if status != "completed" {
		childFailureMetadata(&finish, task.assignment.inheritedAnswer, transcript, exportErr)
	}
	if err != nil {
		finish.Error = err.Error()
	}
	id, commitErr := r.Store.FinishChildTurn(r.Current(), finish)
	if commitErr == nil {
		task.assignment.finish = id
		task.assignment.completion = finish
		task.assignment.pending = nil
		child.state = finish.ContextState
		if child.state == "idle" {
			child.lastAnswer, child.lastTruncated, child.lastResult = finish.Answer, finish.Truncated, finish.ResultEntry
		} else {
			delete(r.children, child.id)
		}
		commitErr = r.publishChildLocked(child.view())
		if commitErr == nil && task.assignment.background {
			body, _ := json.Marshal(childCompletionNotification(finish))
			commitErr = r.queueCommittedNotificationLocked(string(body), id)
		}
	}
	if commitErr != nil {
		child.state = "closed"
		delete(r.children, child.id)
		r.orderError = errors.Join(r.orderError, commitErr)
	}
	r.orderMu.Unlock()
	r.routeMu.RUnlock()
	r.childStartMu.Unlock()
	if commitErr != nil {
		r.Jobs.StopOwned(child.id)
		completionErr := fmt.Errorf(prompts.ChildCompletionCommitFailure, commitErr)
		finish.Error = errors.Join(err, completionErr).Error()
		finish.ContextState = "closed"
		if finish.Outcome == "completed" {
			finish.Outcome = "failed"
			r.routeMu.RLock()
			transcript, exportErr = r.Store.ArchiveActorTranscript(r.Current(), child.id, task.assignment.pending)
			r.routeMu.RUnlock()
			childFailureMetadata(&finish, task.assignment.inheritedAnswer, transcript, exportErr)
		}
		finish.Warning += prompts.ChildCompletionPublicationFailure
		handoffErr := fmt.Errorf("%w; %s", completionErr, finish.Warning)
		// Storage failures still abort the runtime, but the foreground caller
		// must not receive concatenated stdout or lose its investigation paths.
		r.childStartMu.Lock()
		task.assignment.completion = finish
		task.assignment.pending = nil
		r.childStartMu.Unlock()
		r.orderMu.Lock()
		r.orderError = errors.Join(r.orderError, handoffErr)
		r.orderMu.Unlock()
		return handoffErr
	}
	r.emit(Event{Kind: "status", Actor: child.id, EntryID: id, Text: "Assignment · " + render.Status(status)})
	if task.assignment.background {
		r.emit(Event{Kind: "wake", Actor: child.id, Text: "Child turn finished"})
	}
	return nil
}

func childFailureMetadata(finish *history.ChildFinish, inherited bool, transcript string, exportErr error) {
	if finish.Answer == "" {
		finish.Answer = prompts.ChildNoAssistantText
	}
	finish.Warning = fmt.Sprintf(prompts.ChildFailureWarning, finish.Outcome)
	if inherited {
		finish.Warning += prompts.ChildInheritedAnswerWarning
	}
	if exportErr != nil {
		finish.TranscriptExportError = exportErr.Error()
		finish.Warning += fmt.Sprintf(prompts.ChildTranscriptExportFailure, exportErr)
	} else {
		finish.TranscriptPath = transcript
		finish.TranscriptJSONLPath = transcript + ".jsonl"
		finish.Warning += fmt.Sprintf(prompts.ChildTranscriptReferences, transcript, finish.TranscriptJSONLPath)
	}
}

func (r *Runtime) admitChild(ctx context.Context, task childTask, messages []llm.Message, cursor contextCursor) (admitted history.Admission, next contextCursor, err error) {
	next = cursor
	err = r.Workspace.Admit(ctx, func() error {
		r.orderMu.Lock()
		defer r.orderMu.Unlock()
		if r.orderError != nil {
			return r.orderError
		}
		cm, candidate, e := r.runtimeContextLocked(ctx, task.actor, task.selection, cursor)
		if e != nil {
			return e
		}
		system := childSystemTemplate
		if task.aside {
			system = asideSystemTemplate
		}
		var notices []llm.Message
		runtimeCount := 0
		if task.assignment != nil {
			notices = append([]llm.Message(nil), task.assignment.queued...)
			runtimeNotices, count, e := r.runtimeNoticeBatch(task.assignment)
			if e != nil {
				return e
			}
			runtimeCount = count
			notices = append(notices, runtimeNotices...)
			sort.Slice(notices, func(i, j int) bool { return notices[i].EventSeq < notices[j].EventSeq })
		}
		input := r.contextMessages(task.selection, append(append([]llm.Message(nil), messages...), notices...))
		if cm != nil {
			input = append(input, *cm)
		}
		if !contextbuild.Fits(task.selection, system, task.tools.Definitions(), input, false) {
			return errNeedsCompaction
		}
		admitted, e = r.Store.AdmitRequest(r.Current(), task.turn, task.actor, task.selection, cm, notices, messages)
		if e == nil {
			if task.assignment != nil {
				task.assignment.queued = nil
				task.assignment.queuedTokens = 0
				task.assignment.consumeRuntimeNotices(runtimeCount)
			}
			next = candidate
			r.notifications = append(r.notifications, admitted.Notifications...)
		}
		return e
	})
	return
}
