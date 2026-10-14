package jobs

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTaskResultPublishesMetadataWithTerminalCapture(t *testing.T) {
	started, release, published, commit := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce, commitOnce sync.Once
	unblockWorker := func() { releaseOnce.Do(func() { close(release) }) }
	unblockCommit := func() { commitOnce.Do(func() { close(commit) }) }
	notices := make(chan Snapshot, 1)
	m := New(context.Background(), func(v Snapshot) { notices <- v })
	defer m.Close()
	defer unblockWorker()
	defer unblockCommit()
	m.OnState = func(v Snapshot) {
		if v.Status != "running" {
			close(published)
			<-commit
		}
	}
	code := 7
	id, err := m.StartTask("main", "fixture", "result", true, true, func(ctx context.Context, stdout, stderr io.Writer) (TaskResult, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return TaskResult{}, ctx.Err()
		}
		if _, err := io.WriteString(stdout, "final output"); err != nil {
			return TaskResult{}, err
		}
		return TaskResult{ExitCode: &code}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	before, err := m.View("main", id)
	if err != nil || before.Status != "running" || before.ExitCode != nil {
		t.Fatal(before, err)
	}
	unblockWorker()
	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("terminal state not published")
	}
	terminal, err := m.View("main", id)
	if err != nil || terminal.Status != "completed" || terminal.ExitCode == nil || *terminal.ExitCode != 7 || terminal.Stdout != "final output" {
		t.Fatal(terminal, err)
	}
	// The supervisor owns a copied value, not the worker's pointer.
	code = 42
	if *terminal.ExitCode != 7 {
		t.Fatal("worker metadata remained aliased")
	}
	unblockCommit()
	waited, err := m.Wait(context.Background(), "main", id, nil)
	if err != nil || waited.ExitCode == nil || *waited.ExitCode != 7 {
		t.Fatal(waited, err)
	}
	select {
	case notification := <-notices:
		if notification.Status != waited.Status || notification.ExitCode == nil || *notification.ExitCode != 7 || notification.Stdout != waited.Stdout {
			t.Fatal(notification, waited)
		}
	case <-time.After(time.Second):
		t.Fatal("completion notification missing")
	}
}

func TestTaskResultPreservesObservedOutcomeOnWorkerFailure(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	id, err := m.StartTask("main", "fixture", "failure", false, false, func(context.Context, io.Writer, io.Writer) (TaskResult, error) {
		return TaskResult{Signal: "terminated"}, errors.New("supervision failed")
	})
	if err != nil {
		t.Fatal(err)
	}
	v, err := m.Wait(context.Background(), "main", id, nil)
	if err != nil || v.Status != "failed" || v.Signal != "terminated" || v.ExitCode != nil || !strings.Contains(v.Stderr, "supervision failed") {
		t.Fatal(v, err)
	}
}
