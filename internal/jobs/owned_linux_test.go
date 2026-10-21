package jobs

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestOwnedCleanupJoinsPublicationAndClosesRegistration(t *testing.T) {
	notifying, release := make(chan struct{}), make(chan struct{})
	m := New(context.Background(), func(Snapshot) { close(notifying); <-release })
	defer m.Close()
	m.BeginOwned("main/child")
	id, err := m.StartTask("main/child", TaskOptions{Kind: "fixture", Background: true}, func(ctx context.Context, _, _ io.Writer) (TaskResult, error) { return TaskResult{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	<-notifying
	if _, err := m.Wait(context.Background(), "main", id, nil); err != nil {
		t.Fatal(err)
	}
	joined := make(chan struct{})
	go func() { m.StopOwned("main/child"); close(joined) }()
	select {
	case <-joined:
		t.Fatal("cleanup returned before publication finished")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("cleanup failed to join")
	}
	if _, err := m.StartTask("main/child", TaskOptions{Kind: "fixture"}, func(context.Context, io.Writer, io.Writer) (TaskResult, error) {
		t.Error("closed scope ran work")
		return TaskResult{}, nil
	}); err == nil {
		t.Fatal("closed scope accepted registration")
	}
	if _, err := m.StartTask("main/child/new_descendant", TaskOptions{Kind: "fixture"}, func(context.Context, io.Writer, io.Writer) (TaskResult, error) {
		t.Error("closed ancestor ran descendant work")
		return TaskResult{}, nil
	}); err == nil {
		t.Fatal("new descendant bypassed closed ancestor")
	}
}
