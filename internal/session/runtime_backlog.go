package session

import (
	"encoding/json"

	contextbuild "ttc/internal/context"
	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

const maxRuntimeNoticeBatch = 32
const maxRuntimeNoticeTokens = 2048

// pendingRuntimeNotice retains only a durable source reference, not potentially
// large command output or timer bodies. Each admitted reference keeps its own
// source identity for the existing delivery acknowledgment transaction.
type pendingRuntimeNotice struct {
	seq     int64
	timerID string
}

func noticeTokens(message llm.Message) int { return contextbuild.Tokens([]llm.Message{message}) }

func (assignment *childAssignment) queueRuntimeNotice(seq int64, timerID string) {
	if timerID != "" {
		if previous := assignment.runtimeTimers[timerID]; previous != nil {
			previous.seq = seq
			return
		}
	}
	pending := &pendingRuntimeNotice{seq: seq, timerID: timerID}
	assignment.runtimeNotices = append(assignment.runtimeNotices, pending)
	if timerID != "" {
		if assignment.runtimeTimers == nil {
			assignment.runtimeTimers = make(map[string]*pendingRuntimeNotice)
		}
		assignment.runtimeTimers[timerID] = pending
	}
}

// runtimeNoticeBatch materializes only a bounded count/token batch. Remaining
// references stay pending and cause the next inference boundary to continue,
// rather than disappearing into an aggregate with incomplete acknowledgments.
func (r *Runtime) runtimeNoticeBatch(pendingNotices []*pendingRuntimeNotice, tokenLimit int) ([]llm.Message, int, error) {
	var notices []llm.Message
	tokens := 0
	limit := maxRuntimeNoticeTokens
	if tokenLimit > 0 && tokenLimit < limit {
		limit = tokenLimit
	}
	for _, pending := range pendingNotices {
		if len(notices) == maxRuntimeNoticeBatch {
			break
		}
		// The local estimator charges at least one token per three content
		// bytes. A larger durable body cannot fit this batch; fetch only its
		// scalar reference metadata instead of copying megabytes under orderMu.
		message, omitted, err := r.runtimeNoticeWithLimit(pending.seq, 3*limit)
		if err != nil {
			return nil, 0, err
		}
		cost := noticeTokens(message)
		if omitted || cost > limit/2 {
			var ref struct {
				Type             string `json:"type"`
				JobID            string `json:"job_id,omitempty"`
				Kind             string `json:"kind,omitempty"`
				ExitCode         *int   `json:"exit_code,omitempty"`
				Signal           string `json:"signal,omitempty"`
				WakeupID         string `json:"wakeup_id,omitempty"`
				Fired            int    `json:"fired_count,omitempty"`
				Status           string `json:"status,omitempty"`
				ChildID          string `json:"child_id,omitempty"`
				ChildTurnID      string `json:"child_turn_id,omitempty"`
				AssignmentStatus string `json:"assignment_status,omitempty"`
				ChildState       string `json:"child_state,omitempty"`
				ResultEntry      int64  `json:"result_entry_id,omitempty"`
				RecoveredFrom    int64  `json:"recovered_from_event_seq,omitempty"`
				OutputOmitted    bool   `json:"output_omitted"`
				Details          string `json:"details"`
			}
			_ = json.Unmarshal([]byte(message.Content), &ref)
			ref.Details = prompts.RuntimeEventHistoryDetails
			ref.OutputOmitted = true
			body, _ := json.Marshal(ref)
			message.Content = eventContent(string(body), pending.seq, ref.RecoveredFrom)
			cost = noticeTokens(message)
		}
		if len(notices) > 0 && tokens+cost > limit {
			break
		}
		notices = append(notices, message)
		tokens += cost
	}
	return notices, len(notices), nil
}

// runtimeNoticeWithLimit projects bounded canonical source data, preserving
// recovery provenance and child API fields without changing source ownership.
func (r *Runtime) runtimeNoticeWithLimit(seq int64, maxSourceBytes int) (llm.Message, bool, error) {
	source, omitted, err := r.Store.RuntimeNotificationSource(seq, maxSourceBytes)
	if err != nil {
		return llm.Message{}, false, err
	}
	messages, err := recoveredNotificationMessages([]history.PendingNotification{source})
	if err != nil {
		return llm.Message{}, false, err
	}
	return messages[0], omitted, nil
}

func (assignment *childAssignment) consumeRuntimeNotices(count int) {
	for _, pending := range assignment.runtimeNotices[:count] {
		if pending.timerID != "" {
			delete(assignment.runtimeTimers, pending.timerID)
		}
	}
	clear(assignment.runtimeNotices[:count])
	assignment.runtimeNotices = assignment.runtimeNotices[count:]
	if len(assignment.runtimeNotices) == 0 {
		assignment.runtimeNotices = nil
		assignment.runtimeTimers = nil
	}
}
