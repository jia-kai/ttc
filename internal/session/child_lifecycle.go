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
	"ttc/internal/jobs"
	"ttc/internal/llm"
	"ttc/internal/prompts"
	"ttc/internal/render"
	"ttc/internal/tool"
)

// codingChild retains one isolated context. childStartMu protects all fields;
// only its running worker owns messages/cursor until publishing an idle state.
type codingChild struct {
	id, label, state, turn, job string
	selection                   llm.Selection
	tools                       *tool.Registry
	messages                    []llm.Message
	cursor                      contextCursor
	closing                     bool
	lastAnswer                  string // Last nonempty assistant text across persistent assignments, bounded like parent delivery.
	lastResult                  int64
	lastTruncated               bool
}

type childAssignment struct {
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

// ChildViews returns current coding contexts, including idle contexts. Children
// cannot inspect sibling contexts; the main actor sees all of them.
func (r *Runtime) ChildViews(actor string) []tool.ChildView {
	r.childStartMu.Lock()
	defer r.childStartMu.Unlock()
	out := []tool.ChildView{}
	for _, child := range r.children {
		if actor == "main" || actor == child.id {
			out = append(out, tool.ChildView{ID: child.id, Label: child.label, State: child.state, TurnID: child.turn, JobID: child.job})
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
		r.Jobs.StopOwned(id, "")
		r.childStartMu.Lock()
		if err := r.publishChild(tool.ChildView{ID: child.id, Label: child.label, State: "closed", TurnID: child.turn, JobID: child.job}); err != nil {
			r.childStartMu.Unlock()
			return nil, err
		}
		delete(r.children, id)
		r.childStartMu.Unlock()
		return map[string]string{"child_id": id, "state": "closed"}, nil
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
	r.childStartMu.Lock()
	delete(r.children, id)
	r.childStartMu.Unlock()
	return map[string]string{"child_id": id, "state": "closed"}, nil
}

func (r *Runtime) childResult(child *codingChild, assignment *childAssignment, view jobs.Snapshot) map[string]any {
	r.childStartMu.Lock()
	defer r.childStartMu.Unlock()
	data, _ := json.Marshal(view)
	var result map[string]any
	_ = json.Unmarshal(data, &result)
	result["child_id"], result["child_turn_id"] = child.id, assignment.turn
	result["persistent"] = assignment.persistent
	if assignment.background {
		delete(result, "stdout") // Completion notification owns the answer, even for a fast launch.
		return result
	}
	if finish := assignment.completion; finish.Status != "" {
		if assignment.finish != 0 {
			result["finish_event_seq"] = assignment.finish
		}
		result["result_entry_id"] = finish.ResultEntry
		result["status"] = finish.Status
		result["answer"] = finish.Answer
		if finish.Truncated {
			result["answer_truncated"] = true
		}
		for key, value := range map[string]string{
			"warning":                 assignment.completion.Warning,
			"transcript_path":         assignment.completion.TranscriptPath,
			"transcript_jsonl_path":   assignment.completion.TranscriptJSONLPath,
			"transcript_export_error": assignment.completion.TranscriptExportError,
			"error":                   assignment.completion.Error,
		} {
			if value != "" {
				result[key] = value
			}
		}
		delete(result, "stdout") // Return one assistant message, never concatenated commentary.
	}
	return result
}

func (r *Runtime) finishChild(child *codingChild, task childTask, err error) error {
	status := "completed"
	if err != nil {
		status = "failed"
	}
	if task.cancelled {
		status = "cancelled"
	}
	r.childStartMu.Lock()
	closing := child.closing
	r.childStartMu.Unlock()
	if status != "completed" || closing || !task.assignment.persistent {
		r.Jobs.StopOwned(child.id, task.assignment.job)
	}
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
	finish := history.ChildFinish{Type: "child_turn_finished", ChildID: child.id, TurnID: task.assignment.turn, JobID: task.assignment.job, Status: status, Persistent: task.assignment.persistent, ResultEntry: task.assignment.result}
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
		if status == "completed" && !child.closing && task.assignment.persistent {
			child.state = "idle"
			child.lastAnswer, child.lastTruncated, child.lastResult = finish.Answer, finish.Truncated, finish.ResultEntry
		} else {
			child.state = "closed"
			delete(r.children, child.id)
		}
		commitErr = r.publishChildLocked(tool.ChildView{ID: child.id, Label: child.label, State: child.state, TurnID: child.turn, JobID: child.job})
		if commitErr == nil && task.assignment.background {
			body, _ := json.Marshal(finish)
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
		r.Jobs.StopOwned(child.id, task.assignment.job)
		completionErr := fmt.Errorf(prompts.ChildCompletionCommitFailure, commitErr)
		finish.Error = errors.Join(err, completionErr).Error()
		if finish.Status == "completed" {
			finish.Status = "failed"
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
	finish.Warning = fmt.Sprintf(prompts.ChildFailureWarning, finish.Status)
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
			system = systemTemplate
		}
		input := r.contextMessages(task.selection, messages)
		if cm != nil {
			input = append(input, *cm)
		}
		if !contextbuild.Fits(task.selection, system, task.tools.Definitions(), input, false) {
			return errNeedsCompaction
		}
		admitted, e = r.Store.AdmitRequest(r.Current(), task.turn, task.actor, task.selection, cm, nil, messages)
		if e == nil {
			next = candidate
		}
		return e
	})
	return
}
