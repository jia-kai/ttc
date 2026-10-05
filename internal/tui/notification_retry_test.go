package tui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/provider"
	"ttc/internal/session"
)

// notificationRetryProvider gates a failed request without using real deadlines
// or network calls. Requests after the failure use the successful offline script.
type notificationRetryProvider struct {
	provider.Script
	mu        sync.Mutex
	requests  []provider.Request
	failures  map[int]bool // One-based request indexes; initialized before use.
	ready     chan struct{}
	release   chan struct{}
	interrupt bool
}

func (p *notificationRetryProvider) Stream(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	call := len(p.requests)
	p.mu.Unlock()
	if p.failures[call] {
		if call == 2 {
			close(p.ready)
			if p.interrupt {
				<-ctx.Done()
				return ctx.Err()
			}
			select {
			case <-p.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return context.DeadlineExceeded
	}
	return p.Script.Stream(ctx, req, emit)
}

func (p *notificationRetryProvider) snapshot() []provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.Request(nil), p.requests...)
}

func newNotificationRetryUI(t *testing.T, oversized, interrupt bool) (*questionTestUI, *notificationRetryProvider) {
	t.Helper()
	p := &notificationRetryProvider{
		Script: provider.Script{Responses: []provider.ScriptResponse{
			{Text: "Seed completed."},
			{Text: "Research handoff completed."},
			{Text: "Pending notices delivered."},
			{Text: "Fresh notice automatically delivered."},
		}},
		failures: map[int]bool{2: true}, ready: make(chan struct{}), release: make(chan struct{}), interrupt: interrupt,
	}
	u := newQuestionTestUIWithSetup(t, p, nil, func(r *session.Runtime) {
		seed := provider.Message{Role: "user", Content: "Seed research history"}
		if err := r.Run(&seed); err != nil {
			t.Fatal(err)
		}
		if oversized {
			for _, message := range []provider.Message{
				{Role: "user", Content: "Older research task"},
				{Role: "assistant", Content: strings.Repeat("older evidence ", 6000)},
			} {
				if _, err := r.Store.Append(r.Current(), "", "main", "message", message.Role, true, message); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
	return u, p
}

func queueTestNotification(t *testing.T, u *questionTestUI, label string) string {
	t.Helper()
	id, err := u.runtime.Jobs.StartTask("main", "shell", label, true, true, func(context.Context, io.Writer, io.Writer) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func waitNotificationRetryRequest(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("failed provider request did not start")
	}
}

func assertNotificationRetryPaused(t *testing.T, u *questionTestUI, p *notificationRetryProvider, calls int) {
	t.Helper()
	u.wait(t, "Auto-wake paused")
	// Exercise multiple idle scheduler ticks, including fresh runtime events.
	deadline := time.NewTimer(550 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case <-u.screen.frames:
		case <-deadline.C:
			if got := len(p.snapshot()); got != calls {
				t.Fatalf("paused frontend retried provider: got %d calls, want %d", got, calls)
			}
			if !u.runtime.HasNotifications() {
				t.Fatal("failed foreground work consumed queued notifications")
			}
			return
		}
	}
}

func assertNotificationDelivered(t *testing.T, u *questionTestUI, req provider.Request, id string) {
	t.Helper()
	found := 0
	for _, message := range req.Messages {
		if message.Runtime && strings.Contains(message.Content, `"job_id":"`+id+`"`) && strings.Contains(message.Content, `"type":"job_exit"`) {
			found++
		}
	}
	if found != 1 || u.runtime.HasNotifications() {
		t.Fatal("pending notification lost, duplicated, or not consumed", found, u.runtime.HasNotifications())
	}
}

func TestNotificationCompactionFailurePausesAutomaticRetryAndExplicitRecoveryResumes(t *testing.T) {
	for _, recovery := range []string{"compact", "prompt", "model", "load"} {
		t.Run(recovery, func(t *testing.T) {
			u, p := newNotificationRetryUI(t, true, false)
			id := queueTestNotification(t, u, "original completion")
			waitNotificationRetryRequest(t, p.ready)
			if !p.snapshot()[1].NoTools {
				t.Fatal("fixture did not trigger automatic compaction")
			}
			close(p.release)
			assertNotificationRetryPaused(t, u, p, 2)
			fresh := queueTestNotification(t, u, "fresh completion while paused")
			// Inspection is not permission to retry the failed request.
			u.typeText("/jobs")
			u.key(tcell.KeyEnter)
			u.wait(t, "jobs · Esc closes")
			u.key(tcell.KeyEscape)
			assertNotificationRetryPaused(t, u, p, 2)
			before := u.runtime.Current()
			switch recovery {
			case "compact":
				u.typeText("/compact")
			case "prompt":
				u.typeText("Retry the research")
			case "model":
				u.typeText("/model scripted none")
			case "load":
				u.typeText("/load " + before)
			}
			u.key(tcell.KeyEnter)
			if recovery == "compact" {
				u.wait(t, "Command result")
				u.key(tcell.KeyEscape)
			}
			u.wait(t, "Pending notices delivered.")
			u.wait(t, "Turn complete")
			requests := p.snapshot()
			if len(requests) != 4 || !requests[2].NoTools || requests[3].NoTools || u.runtime.Current() == before {
				t.Fatal("explicit recovery did not compact once and deliver", len(requests), u.runtime.Current())
			}
			assertNotificationDelivered(t, u, requests[3], id)
			assertNotificationDelivered(t, u, requests[3], fresh)
			if recovery == "prompt" && latestHumanInput(requests[3]).Content != "Retry the research" {
				t.Fatal("explicit retry prompt was not admitted")
			}
			// A successful explicit recovery must reopen later automatic wakeups.
			next := queueTestNotification(t, u, "completion after recovery")
			u.wait(t, "Fresh notice automatically delivered.")
			u.wait(t, "Turn complete")
			assertNotificationDelivered(t, u, p.snapshot()[4], next)
		})
	}
}

func TestNotificationCompactionInterruptionDoesNotStarveQueuedPrompt(t *testing.T) {
	u, p := newNotificationRetryUI(t, true, true)
	id := queueTestNotification(t, u, "completion before interrupt")
	waitNotificationRetryRequest(t, p.ready)
	u.typeText("Queued retry after interruption")
	u.screen.PostEventWait(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModAlt))
	u.wait(t, "Queued · Queued retry after interruption")
	u.key(tcell.KeyEscape)
	u.key(tcell.KeyEscape)
	u.wait(t, "Pending notices delivered.")
	u.wait(t, "Turn complete")
	requests := p.snapshot()
	if len(requests) != 4 || latestHumanInput(requests[3]).Content != "Queued retry after interruption" {
		t.Fatal("notifications starved queued retry input", len(requests))
	}
	assertNotificationDelivered(t, u, requests[3], id)
}

func TestFailedManualCompactionKeepsAutomaticNotificationsPaused(t *testing.T) {
	u, p := newNotificationRetryUI(t, true, false)
	// Both automatic and first manual compaction fail; only the second manual
	// attempt succeeds. Failed recovery must not reopen notification-only turns.
	p.failures[3] = true
	id := queueTestNotification(t, u, "pending manual recovery")
	waitNotificationRetryRequest(t, p.ready)
	close(p.release)
	assertNotificationRetryPaused(t, u, p, 2)
	u.typeText("/compact")
	u.key(tcell.KeyEnter)
	u.wait(t, "Error: context deadline exceeded")
	assertNotificationRetryPaused(t, u, p, 3)
	u.typeText("/compact")
	u.key(tcell.KeyEnter)
	u.wait(t, "Command result")
	u.key(tcell.KeyEscape)
	u.wait(t, "Pending notices delivered.")
	u.wait(t, "Turn complete")
	assertNotificationDelivered(t, u, p.snapshot()[4], id)
}

func TestForegroundFailurePausesFreshNotificationsUntilUserPrompt(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupt=%v", interrupt), func(t *testing.T) {
			u, p := newNotificationRetryUI(t, false, interrupt)
			u.typeText("Foreground work")
			u.key(tcell.KeyEnter)
			waitNotificationRetryRequest(t, p.ready)
			if interrupt {
				u.key(tcell.KeyEscape)
				u.key(tcell.KeyEscape)
			} else {
				close(p.release)
			}
			u.wait(t, "Auto-wake paused")
			id := queueTestNotification(t, u, "new completion after foreground failure")
			assertNotificationRetryPaused(t, u, p, 2)
			u.typeText("Explicit retry")
			u.key(tcell.KeyEnter)
			u.wait(t, "Research handoff completed.")
			u.wait(t, "Turn complete")
			requests := p.snapshot()
			if len(requests) != 3 || requests[2].NoTools || latestHumanInput(requests[2]).Content != "Explicit retry" {
				t.Fatal("explicit foreground retry did not run", len(requests))
			}
			assertNotificationDelivered(t, u, requests[2], id)
		})
	}
}

func TestAutomaticNotificationsContinueNormallyAfterSuccessfulTurn(t *testing.T) {
	p := &notificationRetryProvider{Script: provider.Script{Responses: []provider.ScriptResponse{
		{Text: "User turn succeeded."}, {Text: "First notification succeeded."}, {Text: "Second notification succeeded."},
	}}}
	u := newQuestionTestUI(t, p)
	u.typeText("Start normally")
	u.key(tcell.KeyEnter)
	u.wait(t, "User turn succeeded.")
	u.wait(t, "Turn complete")
	for i, text := range []string{"First notification succeeded.", "Second notification succeeded."} {
		id := queueTestNotification(t, u, text)
		u.wait(t, text)
		u.wait(t, "Turn complete")
		requests := p.snapshot()
		if len(requests) != i+2 {
			t.Fatal("unexpected automatic request count", len(requests))
		}
		assertNotificationDelivered(t, u, requests[i+1], id)
	}
}
