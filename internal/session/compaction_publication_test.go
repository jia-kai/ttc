package session

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"ttc/internal/history"
	"ttc/internal/jobs"
	"ttc/internal/llm"
)

func TestCompactionIncludesPublicationWhileWaitingForWorkspace(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	compactionBudget(t, r)
	seedCompactionHistory(t, r, strings.Repeat("Earlier research. ", 500))
	before := r.Current()
	r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		return emit(llm.StreamEvent{Kind: "text", Text: "Summary."})
	}}

	held, release, holderDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(holderDone)
		_ = r.Workspace.Admit(context.Background(), func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-holderDone
	}()

	result := make(chan error, 1)
	go func() {
		_, err := r.compactContext(context.Background(), "", r.CurrentSelection(), nil, false)
		result <- err
	}()
	// Wait for the actual blocked admission, not a sleep after inference. The
	// held callback accounts for the first Manager.Admit stack; compaction must
	// be the second before we publish in the previously vulnerable window.
	deadline := time.Now().Add(5 * time.Second)
	for {
		buf := make([]byte, 1<<20)
		stack := string(buf[:runtime.Stack(buf, true)])
		if strings.Count(stack, "ttc/internal/workspace.(*Manager).Admit(") >= 2 && strings.Contains(stack, "ttc/internal/session.(*Runtime).compactContext(") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("compaction did not reach workspace admission")
		}
		time.Sleep(time.Millisecond)
	}
	r.publishJob(jobs.Snapshot{ID: "handoff-job", Owner: "main"})
	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal("normal publication prevented handoff", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("compaction did not finish after releasing workspace")
	}
	saved, err := r.Store.Session(before)
	if err != nil || r.Current() == before || saved.CompactionError != "" || r.checkContext(false) != nil {
		t.Fatal("publication invalidated context or prevented handoff", saved, err)
	}
	exact, err := r.Store.TranscriptJSONL(r.Current(), 0)
	if err != nil || !strings.Contains(string(exact), "handoff-job") {
		t.Fatal("handoff lost the committed publication", err)
	}
}

func TestStaleCompactionPlanIsRecoverable(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Keep sound context")
	before := r.Current()
	if err := r.queueNotification(`{"type":"job_exit","job_id":"keep-pending"}`); err != nil {
		t.Fatal(err)
	}
	stale := fmt.Errorf("concurrent completion: %w", history.ErrCompactionPlanChanged)
	if got := r.compactionFailure(before, stale); !errors.Is(got, history.ErrCompactionPlanChanged) {
		t.Fatal("lost recoverable stale-plan error", got)
	}
	saved, err := r.Store.Session(before)
	if err != nil || saved.CompactionError != "" || r.checkContext(false) != nil || !r.HasNotifications() {
		t.Fatal("stale plan damaged sound context or pending notices", saved, err)
	}
	if recoverableCompaction(errors.Join(stale, errors.New("broken persistence"))) {
		t.Fatal("stale-plan cause masked an independent fatal storage failure")
	}
}
