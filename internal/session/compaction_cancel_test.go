package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ttc/internal/llm"
)

func TestCompactionTimeoutAndOwningCancellation(t *testing.T) {
	for _, actor := range []string{"main", "child", "btw"} {
		for _, mode := range []string{"default-deadline", "inherited-short", "inherited-long", "cancel"} {
			t.Run(actor+"/"+mode, func(t *testing.T) {
				r, _ := runtimeFixture(t, nil)
				compactionBudget(t, r)
				seedCompactionHistory(t, r, strings.Repeat("Earlier evidence. ", 700))
				before := r.Current()
				messages, err := r.Store.Messages(before)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var wantDeadline time.Time
				if strings.HasPrefix(mode, "inherited-") {
					parentDeadline := time.Now().Add(20 * time.Minute)
					if mode == "inherited-short" {
						parentDeadline = time.Now().Add(time.Minute)
						wantDeadline = parentDeadline
					}
					var stop context.CancelFunc
					ctx, stop = context.WithDeadline(ctx, parentDeadline)
					defer stop()
				}
				calls := 0
				r.Provider = &childProvider{stream: func(got context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
					calls++
					deadline, ok := got.Deadline()
					if !ok {
						t.Fatal("compaction has no deadline")
					}
					if !wantDeadline.IsZero() {
						if !deadline.Equal(wantDeadline) {
							t.Fatalf("compaction changed earlier owning deadline: %v; want %v", deadline, wantDeadline)
						}
					} else if remaining := time.Until(deadline); remaining < 9*time.Minute || remaining > 10*time.Minute {
						t.Fatalf("compaction deadline is not ten minutes: remaining %v", remaining)
					}
					if err := emit(llm.StreamEvent{Kind: "text", Text: "Earlier evidence summarized."}); err != nil {
						return err
					}
					if mode == "cancel" {
						cancel()
						<-got.Done()
						return got.Err()
					}
					return emit(llm.StreamEvent{Kind: "completed"})
				}}
				if actor == "main" {
					_, err = r.compactContext(ctx, "", r.CurrentSelection(), nil)
				} else {
					id := "main/" + actor
					turn, e := r.Store.BeginChildTurn(before, id, r.CurrentSelection())
					if e != nil {
						t.Fatal(e)
					}
					_, _, err = r.compactChild(ctx, childTask{actor: id, turn: turn, selection: r.CurrentSelection(), tools: r.Tools, aside: actor == "btw"}, messages, contextCursor{}, nil)
				}
				if calls != 1 {
					t.Fatal("expected one summary request", calls)
				}
				if mode == "cancel" {
					if !errors.Is(err, context.Canceled) || r.Current() != before || r.checkContext() != nil || r.HasNotifications() {
						t.Fatal("cancellation committed a handoff or invalidated context", err)
					}
				} else if err != nil {
					t.Fatal(err)
				} else if actor == "main" && r.Current() == before {
					t.Fatal("successful summary did not create continuation")
				}
			})
		}
	}
}

func TestCanceledCompactionCanReloadCompactAndContinue(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	compactionBudget(t, r)
	seedCompactionHistory(t, r, strings.Repeat("Earlier evidence. ", 700))
	before := r.Current()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Provider = &childProvider{stream: func(ctx context.Context, _ llm.Request, _ func(llm.StreamEvent) error) error {
		cancel()
		return ctx.Err()
	}}
	if _, err := r.compactContext(ctx, "", r.CurrentSelection(), nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := r.Command("/load " + before); err != nil {
		t.Fatal(err)
	}
	loaded := r.Current()
	if loaded == before {
		t.Fatal("load did not copy writable history")
	}
	summaries, coding := 0, 0
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		if req.NoTools {
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) < 9*time.Minute || time.Until(deadline) > 10*time.Minute {
				t.Fatal("manual compaction does not use ten-minute timeout")
			}
			summaries++
			return emit(llm.StreamEvent{Kind: "text", Text: "Earlier evidence summarized; continue verification."})
		}
		coding++
		return emit(llm.StreamEvent{Kind: "text", Text: "Verification continued."})
	}}
	if _, err := r.Command("/compact"); err != nil {
		t.Fatal(err)
	}
	if r.Current() == loaded {
		t.Fatal("manual compaction did not create continuation")
	}
	if err := r.Run(&llm.Message{Role: "user", Content: "Continue verification."}); err != nil {
		t.Fatal(err)
	}
	if summaries != 1 || coding != 1 {
		t.Fatal("unexpected recovery inference", summaries, coding)
	}
}
