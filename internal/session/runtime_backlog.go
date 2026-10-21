package session

import (
	"encoding/json"

	contextbuild "ttc/internal/context"
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
func (r *Runtime) runtimeNoticeBatch(assignment *childAssignment) ([]llm.Message, int, error) {
	var notices []llm.Message
	tokens := 0
	limit := maxRuntimeNoticeTokens
	if assignment.queueTokenLimit > 0 && assignment.queueTokenLimit < limit {
		limit = assignment.queueTokenLimit
	}
	for _, pending := range assignment.runtimeNotices {
		if len(notices) == maxRuntimeNoticeBatch {
			break
		}
		source, err := r.Store.Entry(pending.seq)
		if err != nil {
			return nil, 0, err
		}
		var event struct {
			Body json.RawMessage `json:"body"`
		}
		if err := json.Unmarshal(source.Content, &event); err != nil {
			return nil, 0, err
		}
		message := llm.Message{Type: llm.TypeRuntimeNotification, EventSeq: pending.seq, Content: eventContent(string(event.Body), pending.seq, 0)}
		cost := noticeTokens(message)
		if cost > limit/2 {
			var ref struct {
				Type          string `json:"type"`
				JobID         string `json:"job_id,omitempty"`
				Kind          string `json:"kind,omitempty"`
				ExitCode      *int   `json:"exit_code,omitempty"`
				Signal        string `json:"signal,omitempty"`
				WakeupID      string `json:"wakeup_id,omitempty"`
				Fired         int    `json:"fired_count,omitempty"`
				Status        string `json:"status,omitempty"`
				OutputOmitted bool   `json:"output_omitted"`
				Details       string `json:"details"`
			}
			_ = json.Unmarshal(event.Body, &ref)
			ref.Details = prompts.RuntimeEventHistoryDetails
			ref.OutputOmitted = true
			body, _ := json.Marshal(ref)
			message.Content = eventContent(string(body), pending.seq, 0)
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
