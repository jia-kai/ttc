package session

import (
	"encoding/json"
	"fmt"

	"ttc/internal/capture"
	"ttc/internal/history"
	"ttc/internal/jobs"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// childCompletionFields is the model-facing assignment contract. It deliberately
// does not embed history.ChildFinish: storage JSON is not an API definition.
type childCompletionFields struct {
	ChildID               string  `json:"child_id"`
	TurnID                string  `json:"child_turn_id"`
	JobID                 string  `json:"job_id"`
	AssignmentStatus      string  `json:"assignment_status"`
	ChildState            string  `json:"child_state,omitempty"` // Unknown migrated state is omitted, not inferred.
	Persistent            bool    `json:"persistent"`
	ResultEntry           *int64  `json:"result_entry_id,omitempty"`
	Answer                *string `json:"answer,omitempty"` // Nil on launch; non-nil preserves an observed empty answer.
	Truncated             bool    `json:"answer_truncated,omitempty"`
	Error                 string  `json:"error,omitempty"`
	Warning               string  `json:"warning,omitempty"`
	TranscriptPath        string  `json:"transcript_path,omitempty"`
	TranscriptJSONLPath   string  `json:"transcript_jsonl_path,omitempty"`
	TranscriptExportError string  `json:"transcript_export_error,omitempty"`
}

// childNotification is the API envelope used for both live and recovered
// completions. Its fields are projected explicitly from the canonical record.
type childNotification struct {
	Type string `json:"type"`
	childCompletionFields
}

// childToolResult adds an explicit allowlist of execution metadata to the child
// API. New jobs.Snapshot fields cannot leak into this response automatically.
// Captured stderr remains inspector/job_read data, never an automatic handoff.
type childToolResult struct {
	childCompletionFields
	Kind            string             `json:"kind"`
	Owner           string             `json:"owner_actor_id"`
	Label           string             `json:"label"`
	StartedAt       string             `json:"started_at"`
	FinishedAt      string             `json:"finished_at,omitempty"`
	ExitCode        *int               `json:"exit_code,omitempty"`
	Signal          string             `json:"signal,omitempty"`
	Stdout          *string            `json:"stdout,omitempty"` // Only a running foreground preview, never a background answer.
	Truncated       bool               `json:"truncated"`
	StdoutSelection *capture.Selection `json:"stdout_selection,omitempty"`
	FinishEventSeq  int64              `json:"finish_event_seq,omitempty"`
}

func childCompletionAPI(record history.ChildFinish) childCompletionFields {
	state := record.ContextState
	if state == "unknown" {
		state = ""
	}
	return childCompletionFields{
		ChildID: record.ChildID, TurnID: record.TurnID, JobID: record.JobID,
		AssignmentStatus: record.Outcome, ChildState: state, Persistent: record.Persistent,
		ResultEntry: &record.ResultEntry, Answer: &record.Answer, Truncated: record.Truncated,
		Error: record.Error, Warning: record.Warning,
		TranscriptPath: record.TranscriptPath, TranscriptJSONLPath: record.TranscriptJSONLPath,
		TranscriptExportError: record.TranscriptExportError,
	}
}

func childCompletionNotification(record history.ChildFinish) childNotification {
	return childNotification{Type: "child_turn_finished", childCompletionFields: childCompletionAPI(record)}
}

func (r *Runtime) childResult(child *codingChild, assignment *childAssignment, view jobs.Snapshot) childToolResult {
	r.childStartMu.Lock()
	defer r.childStartMu.Unlock()
	result := childToolResult{
		childCompletionFields: childCompletionFields{
			ChildID: child.id, TurnID: assignment.turn, JobID: view.ID,
			AssignmentStatus: view.Status, ChildState: child.state, Persistent: assignment.persistent,
		},
		Kind: view.Kind, Owner: view.Owner, Label: view.Label,
		StartedAt: view.StartedAt, FinishedAt: view.FinishedAt, Signal: view.Signal,
	}
	if view.ExitCode != nil {
		code := *view.ExitCode
		result.ExitCode = &code
	}
	if finish := assignment.completion; finish.Outcome != "" {
		// Completion belongs to this assignment even if the context was resumed.
		completion := childCompletionAPI(finish)
		result.AssignmentStatus, result.ChildState = completion.AssignmentStatus, completion.ChildState
		if !assignment.background {
			result.childCompletionFields = completion
			result.FinishEventSeq = assignment.finish
		}
	} else if !assignment.background {
		result.Stdout = &view.Stdout
		// Snapshot.Truncated includes stderr, which is not delivered here.
		result.Truncated = view.StdoutTruncated
		if view.StdoutSelection != nil {
			selection := *view.StdoutSelection
			result.StdoutSelection = &selection
			result.Truncated = selection.OmittedBytes > 0 || selection.CleaningError != ""
		}
	}
	return result
}

// recoveredNotificationMessages is the history-to-model boundary. History owns
// pending event identities and canonical bodies; session owns API serialization.
func recoveredNotificationMessages(pending []history.PendingNotification) ([]llm.Message, error) {
	messages := make([]llm.Message, 0, len(pending))
	for _, event := range pending {
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(event.Body, &header); err != nil {
			return nil, fmt.Errorf(prompts.SessionRecoveredNotificationProjection, event.EventSeq, err)
		}
		body := event.Body
		if header.Type == "child_turn_finished" {
			var record history.ChildFinish
			if err := json.Unmarshal(body, &record); err != nil {
				return nil, fmt.Errorf(prompts.SessionRecoveredNotificationProjection, event.EventSeq, err)
			}
			var err error
			notice := childCompletionNotification(record)
			body, err = json.Marshal(notice)
			if err != nil {
				return nil, fmt.Errorf(prompts.SessionRecoveredNotificationProjection, event.EventSeq, err)
			}
		}
		messages = append(messages, llm.Message{Type: llm.TypeRuntimeNotification, EventSeq: event.EventSeq, Content: eventContent(string(body), event.EventSeq, event.RecoveredFrom)})
	}
	return messages, nil
}
