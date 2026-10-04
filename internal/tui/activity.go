package tui

import (
	"fmt"
	"time"

	"ttc/internal/session"
)

type questionActivity struct {
	id        string
	started   time.Time
	dismissed bool
}

// turnActivity tracks main-agent waits and user-dismissed live questions. Other
// helper activity and historical messages never affect the foreground phase.
type turnActivity struct {
	question                questionActivity
	retryStarted, retryEnds time.Time
}

func questionStatus(form session.QuestionForm) string {
	if form.Dismissed {
		return "What would you like to do instead? · " + form.ID
	}
	return "Waiting for answer · " + form.ID
}

// syncQuestion uses live state rather than publication-time event snapshots.
func (a *turnActivity) syncQuestion(form *session.QuestionForm, now time.Time) {
	if form == nil {
		a.question = questionActivity{}
		return
	}
	if a.question.id != form.ID {
		a.question = questionActivity{id: form.ID, started: now}
	}
	a.question.dismissed = form.Dismissed
}

func (a *turnActivity) observe(event session.Event, now time.Time) {
	if event.Actor != "" && event.Actor != "main" {
		return
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
	if q := a.question; q.id != "" {
		if q.dismissed {
			return "What would you like to do instead?"
		}
		label, since = "Waiting for answer", q.started
	}
	if label == "Working" && now.Before(a.retryEnds) {
		label, since = "Retrying", a.retryStarted
	}
	return fmt.Sprintf("%s · %ds", label, max(0, int(now.Sub(since).Seconds())))
}
