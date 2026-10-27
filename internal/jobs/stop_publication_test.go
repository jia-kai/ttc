package jobs

import (
	"context"
	"io"
	"sync"
	"testing"
	"testing/synctest"
)

func TestStopJoinsCompletionPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		notifying, release := make(chan struct{}), make(chan struct{})
		m := New(context.Background(), func(Snapshot) {
			close(notifying)
			<-release
		})
		defer m.Close()
		// Release a blocked callback even if an assertion fails, before Close joins it.
		releaseNotification := sync.OnceFunc(func() { close(release) })
		defer releaseNotification()
		id, err := m.StartTask("main", TaskOptions{Kind: "fixture", Background: true}, func(ctx context.Context, _, _ io.Writer) (TaskResult, error) {
			<-ctx.Done()
			return TaskResult{}, ctx.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			view, err := m.Stop("main", id)
			if err != nil || view.Status != "cancelled" {
				t.Errorf("stop = %+v, %v", view, err)
			}
		}()
		<-notifying
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("Stop returned before the completion notification was published")
		default:
		}
		releaseNotification()
		<-stopped
	})
}
