package session

import (
	"errors"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

const maxRuntimeNoticeBatch = 32
const maxRuntimeNoticeTokens = 2048

// runtimeNoticeBacklog owns pending durable references, never source payloads.
// Its zero value is ready to use. Callers serialize all operations with orderMu,
// including batch selection, durable admission and consumption. Its indexes
// and ordered nodes have one owner; copies must not be independently mutated.
type runtimeNoticeBacklog struct {
	first, last *runtimeNoticeRef
	sources     map[int64]*runtimeNoticeRef
	timers      map[string]*runtimeNoticeRef
}

type runtimeNoticeRef struct {
	seq        int64
	timerID    string
	prev, next *runtimeNoticeRef
}

func (q *runtimeNoticeBacklog) len() int { return len(q.sources) }

// enqueue preserves source order, including delayed completion publication.
// Ordered arrivals, including timer replacement, are O(1). Delayed sources scan
// backward over newer references (O(n), quadratic for a reverse-ordered burst).
// Timers retain only their newest source, at that source's position rather than
// the first firing's position. Otherwise a recent firing could jump older
// sources across a bounded admission boundary. Stale firings cannot replace it.
func (q *runtimeNoticeBacklog) enqueue(seq int64, timerID string) error {
	if seq <= 0 {
		return errors.New(prompts.SessionNotificationSource)
	}
	if q.sources[seq] != nil {
		return errors.New(prompts.SessionEventAlreadyPending)
	}
	if old := q.timers[timerID]; timerID != "" && old != nil {
		if old.seq > seq {
			return nil
		}
		q.remove(old)
	}
	node := &runtimeNoticeRef{seq: seq, timerID: timerID}
	previous := q.last
	for previous != nil && previous.seq > seq {
		previous = previous.prev
	}
	if previous == nil {
		node.next = q.first
		q.first = node
	} else {
		node.prev, node.next = previous, previous.next
		previous.next = node
	}
	if node.next == nil {
		q.last = node
	} else {
		node.next.prev = node
	}
	if q.sources == nil {
		q.sources = make(map[int64]*runtimeNoticeRef)
	}
	q.sources[seq] = node
	if timerID != "" {
		if q.timers == nil {
			q.timers = make(map[string]*runtimeNoticeRef)
		}
		q.timers[timerID] = node
	}
	return nil
}

func (q *runtimeNoticeBacklog) remove(node *runtimeNoticeRef) {
	if node.prev == nil {
		q.first = node.next
	} else {
		node.prev.next = node.next
	}
	if node.next == nil {
		q.last = node.prev
	} else {
		node.next.prev = node.prev
	}
	delete(q.sources, node.seq)
	if node.timerID != "" {
		delete(q.timers, node.timerID)
	}
}

// consume acknowledges the prefix selected by batch only after durable request
// admission succeeds, while still holding orderMu. Failure leaves it untouched.
func (q *runtimeNoticeBacklog) consume(count int) {
	if count < 0 || count > q.len() {
		panic("runtime notice consumption exceeds pending batch")
	}
	for range count {
		q.remove(q.first)
	}
	if q.first == nil {
		*q = runtimeNoticeBacklog{}
	}
}

func noticeTokens(message llm.Message) int { return contextbuild.Tokens([]llm.Message{message}) }

// batch materializes a bounded count/token prefix without consuming references.
// Remaining sources stay pending for the next inference boundary. One reference
// is allowed even below its metadata cost, so a small limit cannot starve work.
func (q *runtimeNoticeBacklog) batch(project func(int64, int) (llm.Message, error), tokenLimit int) ([]llm.Message, int, error) {
	var notices []llm.Message
	tokens := 0
	limit := maxRuntimeNoticeTokens
	if tokenLimit > 0 && tokenLimit < limit {
		limit = tokenLimit
	}
	for pending := q.first; pending != nil; pending = pending.next {
		if len(notices) == maxRuntimeNoticeBatch {
			break
		}
		message, err := project(pending.seq, limit)
		if err != nil {
			return nil, 0, err
		}
		cost := noticeTokens(message)
		if len(notices) > 0 && tokens+cost > limit {
			break
		}
		notices = append(notices, message)
		tokens += cost
	}
	return notices, len(notices), nil
}
