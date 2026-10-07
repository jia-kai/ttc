package session

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
)

func holdInputAdmission(t *testing.T, r *Runtime) func() {
	t.Helper()
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- r.Workspace.Admit(context.Background(), func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	var once sync.Once
	unlock := func() {
		once.Do(func() {
			close(release)
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(unlock)
	return unlock
}

func finishInputRun(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("input run did not finish")
	}
}

func waitInitialInputRun(t *testing.T, r *Runtime) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		r.mu.Lock()
		started := r.activeCancel != nil
		r.mu.Unlock()
		if started {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not reach initial admission")
		}
		time.Sleep(time.Millisecond)
	}
}

func assertInputHistoryCounts(t *testing.T, r *Runtime, want int) {
	t.Helper()
	for _, query := range []string{
		"SELECT count(*) FROM sessions",
		"SELECT count(*) FROM turns",
		"SELECT count(*) FROM entries WHERE role='user' AND model_visible=1",
		"SELECT count(*) FROM model_requests",
	} {
		var got int
		if err := r.Store.DB.QueryRow(query).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s = %d, want %d", query, got, want)
		}
	}
}

func TestInputCancellationBeforeInitialAdmissionLeavesNoHistory(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	unlock := holdInputAdmission(t, r)
	original := contextbuild.Input{Text: "  original\n\tλ  ", Attachments: []contextbuild.Attachment{{Path: "removed.txt", Kind: "text", Text: "immutable snapshot"}}}
	input := r.PrepareInput(original)
	done := make(chan error, 1)
	go func() { done <- r.RunInput(input) }()
	waitInitialInputRun(t, r)
	assertInputHistoryCounts(t, r, 0)
	restored, err := input.Cancel()
	if err != nil || !reflect.DeepEqual(restored, original) {
		t.Fatal("original input was not restored", restored, err)
	}
	if _, err := input.Cancel(); err == nil {
		t.Fatal("input cancelled twice")
	}
	unlock()
	finishInputRun(t, done)
	assertInputHistoryCounts(t, r, 0)
	if len(events) != 0 {
		t.Fatal("cancelled initial input emitted persisted turn events")
	}
	if input.pending || input.input.Text != "" || input.input.Attachments != nil {
		t.Fatal("ticket retained consumed input")
	}
}

type inputAdmissionProvider struct {
	llm.Script
	ready, release chan struct{}
}

func (p *inputAdmissionProvider) Stream(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
	close(p.ready)
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return p.Script.Stream(ctx, req, emit)
}

func TestInputCancellationAfterInitialAdmissionDoesNotInterruptTurn(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	p := &inputAdmissionProvider{Script: llm.Script{Responses: []llm.ScriptResponse{{Text: "Completed normally."}}}, ready: make(chan struct{}), release: make(chan struct{})}
	r.Provider = p
	input := r.PrepareInput(contextbuild.Input{Text: "admit me"})
	done := make(chan error, 1)
	go func() { done <- r.RunInput(input) }()
	select {
	case <-p.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not start")
	}
	if _, err := input.Cancel(); err == nil {
		t.Fatal("already admitted input was cancelled")
	}
	close(p.release)
	finishInputRun(t, done)
	assertInputHistoryCounts(t, r, 1)
	var status string
	if err := r.Store.DB.QueryRow("SELECT status FROM turns").Scan(&status); err != nil || status != "completed" {
		t.Fatal("losing cancellation interrupted the turn", status, err)
	}
}

func TestInputCancellationRacesInitialAdmission(t *testing.T) {
	for i := 0; i < 24; i++ {
		r, _ := runtimeFixture(t, []llm.ScriptResponse{{Text: "Admitted."}})
		unlock := holdInputAdmission(t, r)
		input := r.PrepareInput(contextbuild.Input{Text: "race"})
		done := make(chan error, 1)
		go func() { done <- r.RunInput(input) }()
		waitInitialInputRun(t, r)
		cancelled := make(chan bool, 1)
		go func() {
			_, err := input.Cancel()
			cancelled <- err == nil
		}()
		unlock()
		won := <-cancelled
		finishInputRun(t, done)
		want := 1
		if won {
			want = 0
		}
		assertInputHistoryCounts(t, r, want)
	}
}

func TestInputCancellationDoesNotInterruptNotificationTurn(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "prior human input")
	p := &inputAdmissionProvider{Script: llm.Script{Responses: []llm.ScriptResponse{{Text: "Notification completed."}}}, ready: make(chan struct{}), release: make(chan struct{})}
	r.Provider = p
	done := make(chan error, 1)
	go func() { done <- r.Run(nil) }()
	select {
	case <-p.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("notification provider did not start")
	}
	input := r.PrepareInput(contextbuild.Input{Text: "unadmitted human input"})
	if _, err := input.Cancel(); err != nil {
		t.Fatal(err)
	}
	close(p.release)
	finishInputRun(t, done)
	if err := r.RunInput(input); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := r.Store.DB.QueryRow("SELECT status FROM turns WHERE trigger='async'").Scan(&status); err != nil || status != "completed" {
		t.Fatal("human input cancellation interrupted notification turn", status, err)
	}
	var humans int
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM entries WHERE role='user' AND model_visible=1").Scan(&humans); err != nil || humans != 1 {
		t.Fatal("cancelled input was admitted with notification", humans, err)
	}
}

func TestInputCancellationExplicitSessionResetDiscardsTicket(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	input := r.PrepareInput(contextbuild.Input{Text: "old generation"})
	if _, err := r.Command("/new"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Cancel(); err == nil {
		t.Fatal("old generation restored into new session")
	}
	if err := r.RunInput(input); err != nil {
		t.Fatal(err)
	}
	assertInputHistoryCounts(t, r, 0)
}
