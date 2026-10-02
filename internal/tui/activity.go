package tui

import (
	"fmt"
	"time"

	"scicode/internal/session"
)

// turnActivity belongs to the frontend. It tracks current main-agent waits,
// never historical messages or background helper activity.
type turnActivity struct {
	questions               map[string]time.Time
	retryStarted, retryEnds time.Time
}

func (a *turnActivity) observe(event session.Event, now time.Time) {
	if event.Kind == "question_closed" && event.Question != nil {
		delete(a.questions, event.Question.ID)
		return
	}
	if event.Actor != "" && event.Actor != "main" {
		return
	}
	if event.Kind == "question" && event.Question != nil && event.Question.Actor == "main" {
		if a.questions == nil {
			a.questions = map[string]time.Time{}
		}
		if _, exists := a.questions[event.Question.ID]; !exists {
			a.questions[event.Question.ID] = now
		}
	}
	if event.Retry != nil {
		a.retryStarted = now
		a.retryEnds = now.Add(time.Duration(event.Retry.DelayMilliseconds) * time.Millisecond)
	}
	switch event.Kind {
	case "delta", "assistant", "tool_pending", "tool_stream_end":
		a.retryStarted, a.retryEnds = time.Time{}, time.Time{}
	}
}

func (a *turnActivity) indicator(now, started time.Time) string {
	label, since := "Working", started
	for _, at := range a.questions {
		if label != "Waiting for answer" || at.Before(since) {
			label, since = "Waiting for answer", at
		}
	}
	if label == "Working" && now.Before(a.retryEnds) {
		label, since = "Retrying", a.retryStarted
	}
	return fmt.Sprintf("%s · %ds", label, max(0, int(now.Sub(since).Seconds())))
}
