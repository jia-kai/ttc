package session

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ttc/internal/llm"
	"ttc/internal/prompts"
)

func TestNamingStartsAtFirstToolBoundaryAndFailureIsVisible(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			r, events := runtimeFixture(t, nil)
			r.AutoName = true
			ready := make(chan struct{})
			var coding atomic.Int32
			r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				if req.NoTools {
					if req.System != prompts.Naming || req.OutputTokens != 32 {
						t.Errorf("naming request instructions or token limit changed: system %q, output tokens %d", req.System, req.OutputTokens)
					}
					if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) <= 14*time.Second || time.Until(deadline) > 15*time.Second {
						t.Errorf("naming request must have a 15-second deadline, got %v (present: %v)", deadline, ok)
					}
					if !strings.Contains(req.Messages[0].Content, "Tool call: **glob** · ` *.go `") {
						return errors.New("naming omitted first tool")
					}
					if fail {
						return errors.New("controlled naming failure")
					}
					if err := emit(llm.StreamEvent{Kind: "text", Text: "Small Research Tool Session"}); err != nil {
						return err
					}
					return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: 20, OutputTokens: 4}})
				}
				if coding.Add(1) == 1 {
					call := llm.ToolCall{ID: "glob", Name: "glob", Arguments: []byte(`{"pattern":"*.go"}`)}
					if err := emit(llm.StreamEvent{Kind: "call", Call: &call}); err != nil {
						return err
					}
					return emit(llm.StreamEvent{Kind: "completed", Usage: &llm.Usage{InputTokens: 100, OutputTokens: 10}})
				}
				close(ready)
				<-ctx.Done()
				return ctx.Err()
			}}
			done := make(chan error, 1)
			go func() { m := llm.Message{Type: llm.TypeUserInput, Content: "Study local source"}; done <- r.Run(&m) }()
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
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		if req.System == prompts.Naming {
			close(namingStarted)
			defer close(namingStopped)
			<-ctx.Done()
			return ctx.Err()
		}
		if req.NoTools {
			summaryCalls.Add(1)
			return errors.New("canceled compaction must not start inference")
		}
		if err := emit(llm.StreamEvent{Kind: "text", Text: "Checked the project."}); err != nil {
			return err
		}
		return emit(llm.StreamEvent{Kind: "completed"})
	}}
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Check the project."}); err != nil {
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
		_, err := r.compactContext(ctx, "", r.CurrentSelection(), nil, false)
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

func TestNamingUsesEasySelectionOrFallsBack(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "fallback", true: "easy"}[configured], func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.AutoName = true
			want := r.CurrentSelection()
			if configured {
				want.Model.ID = "easy-fixture"
				want.Variant = "easy-variant"
				if err := r.RestoreEasyModel(want); err != nil {
					t.Fatal(err)
				}
			}
			selected := make(chan llm.Selection, 1)
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				if req.NoTools {
					selected <- req.Selection
					return emit(llm.StreamEvent{Kind: "text", Text: "Project Research Session"})
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "I will inspect the project."})
			}}
			if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Inspect the project"}); err != nil {
				t.Fatal(err)
			}
			<-r.namingDone
			got := <-selected
			if got.Provider != want.Provider || got.Model.ID != want.Model.ID || got.Variant != want.Variant {
				t.Fatalf("naming selection = %+v, want %+v", got, want)
			}
		})
	}
}
