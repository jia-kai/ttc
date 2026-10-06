package session

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ttc/internal/provider"
)

func TestNamingStartsAtFirstToolBoundaryAndFailureIsVisible(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			r, events := runtimeFixture(t, nil)
			r.AutoName = true
			ready := make(chan struct{})
			var coding atomic.Int32
			r.Provider = &childProvider{stream: func(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
				if req.NoTools {
					if !strings.Contains(req.Messages[0].Content, "Tool: glob") {
						return errors.New("naming omitted first tool")
					}
					if fail {
						return errors.New("controlled naming failure")
					}
					if err := emit(provider.StreamEvent{Kind: "text", Text: "Small Research Tool Session"}); err != nil {
						return err
					}
					return emit(provider.StreamEvent{Kind: "completed", Usage: &provider.Usage{InputTokens: 20, OutputTokens: 4}})
				}
				if coding.Add(1) == 1 {
					call := provider.ToolCall{ID: "glob", Name: "glob", Arguments: []byte(`{"pattern":"*.go"}`)}
					if err := emit(provider.StreamEvent{Kind: "call", Call: &call}); err != nil {
						return err
					}
					return emit(provider.StreamEvent{Kind: "completed", Usage: &provider.Usage{InputTokens: 100, OutputTokens: 10}})
				}
				close(ready)
				<-ctx.Done()
				return ctx.Err()
			}}
			done := make(chan error, 1)
			go func() { m := provider.Message{Role: "user", Content: "Study local source"}; done <- r.Run(&m) }()
			select {
			case <-ready:
			case <-time.After(3 * time.Second):
				t.Fatal("coding not continued")
			}
			<-r.namingDone
			saved, err := r.Store.Session(r.Current())
			if err != nil {
				t.Fatal(err)
			}
			if !fail && saved.Name != "Small Research Tool Session" {
				t.Fatal(saved.Name)
			}
			found := false
			for len(events) > 0 {
				event := <-events
				if fail && strings.Contains(event.Text, "Session naming failed:") || !fail && event.Kind == "session_name" {
					found = true
					if event.EntryID != 0 {
						entry, err := r.Store.Entry(event.EntryID)
						if err != nil || entry.Visible {
							t.Fatal(entry, err)
						}
					}
				}
			}
			if !found {
				t.Fatal("naming outcome not visible")
			}
			select {
			case err := <-done:
				t.Fatal("naming finished or blocked main", err)
			default:
			}
			r.Interrupt()
			<-done
		})
	}
}

func TestCompactionCancellationDoesNotWaitForBackgroundNaming(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.AutoName = true
	namingStarted := make(chan struct{})
	namingStopped := make(chan struct{})
	var summaryCalls atomic.Int32
	r.Provider = &childProvider{stream: func(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
		if strings.HasPrefix(req.System, "Name this coding session") {
			close(namingStarted)
			defer close(namingStopped)
			<-ctx.Done()
			return ctx.Err()
		}
		if req.NoTools {
			summaryCalls.Add(1)
			return errors.New("canceled compaction must not start inference")
		}
		if err := emit(provider.StreamEvent{Kind: "text", Text: "Checked the project."}); err != nil {
			return err
		}
		return emit(provider.StreamEvent{Kind: "completed"})
	}}
	if err := r.Run(&provider.Message{Role: "user", Content: "Check the project."}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-namingStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("background naming did not start")
	}
	before := r.Current()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := r.compactContext(ctx, "", r.CurrentSelection(), nil)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		// Join both workers before reporting a regression, so fixture cleanup
		// cannot race a compaction still touching the history store.
		r.stopNaming()
		<-done
		t.Fatal("canceled compaction remained blocked on naming")
	}
	if r.Current() != before || summaryCalls.Load() != 0 {
		t.Fatal("canceled compaction changed history or started inference")
	}
	select {
	case <-namingStopped:
		t.Fatal("compaction cancellation stopped independent naming")
	default:
	}
	r.stopNaming()
	select {
	case <-namingStopped:
	default:
		t.Fatal("naming shutdown did not join the provider")
	}
}
