package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"ttc/internal/history"
	"ttc/internal/provider"
	"ttc/internal/tool"
)

func TestLoadCompactionBoundaryDeliversPendingChildAnswer(t *testing.T) {
	for _, startup := range []bool{false, true} {
		for _, manual := range []bool{false, true} {
			t.Run(fmt.Sprintf("startup=%v/manual=%v", startup, manual), func(t *testing.T) {
				r, _ := runtimeFixture(t, nil)
				r.Emit = nil
				compactionBudget(t, r)
				seedCompactionHistory(t, r, strings.Repeat("Earlier evidence. ", 700))
				source := r.Current()
				var finish, result int64
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				r.Provider = &childProvider{stream: func(ctx context.Context, _ provider.Request, _ func(provider.StreamEvent) error) error {
					actor := "main/child_completed"
					turn, err := r.Store.BeginChildTurn(source, actor, r.CurrentSelection())
					if err != nil {
						return err
					}
					result, err = r.Store.Append(source, turn, actor, "message", "assistant", false, provider.Message{Role: "assistant", Content: "Storage verified; 35 tests passed."})
					if err != nil {
						return err
					}
					finish, err = r.Store.FinishChildTurn(source, history.ChildFinish{ChildID: actor, TurnID: turn, JobID: "job_completed", Status: "completed", ResultEntry: result, Answer: "Storage verified; 35 tests passed."})
					cancel()
					if err != nil {
						return err
					}
					return ctx.Err()
				}}
				if _, err := r.compactContext(ctx, "", r.CurrentSelection(), nil); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if startup {
					loaded, err := r.Store.Load(source)
					if err != nil {
						t.Fatal(err)
					}
					r.Close()
					restarted, err := New(context.Background(), r.Store, r.Workspace, r.Provider, r.CurrentSelection(), loaded.ID, r.Skills, tool.WebSearchConfig{}, nil)
					if err != nil {
						t.Fatal(err)
					}
					r = restarted
					r.AutoName = false
					t.Cleanup(r.Close)
				} else if _, err := r.Command("/load " + source); err != nil {
					t.Fatal(err)
				}
				if !r.HasNotifications() || len(r.ChildViews("main")) != 0 || len(r.Jobs.Live()) != 0 {
					t.Fatal("load did not recover notices independently of live work")
				}
				var summaries, coding int
				var delivered int64
				r.Provider = &childProvider{stream: func(_ context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
					if req.NoTools {
						summaries++
						return emit(provider.StreamEvent{Kind: "text", Text: "Earlier evidence summarized."})
					}
					coding++
					found := 0
					for _, message := range req.Messages {
						if !message.Runtime || message.Role != "user" {
							continue
						}
						var notice history.ChildFinish
						if json.Unmarshal([]byte(message.Content), &notice) == nil && notice.Type == "child_turn_finished" {
							found++
							if notice.ResultEntry != result || notice.Answer != "Storage verified; 35 tests passed." || message.EventSeq == finish {
								t.Fatal("lost answer or reused source ownership", message)
							}
							delivered = message.EventSeq
						}
					}
					if found != 1 {
						t.Fatal("child answer was not delivered exactly once", found)
					}
					return emit(provider.StreamEvent{Kind: "text", Text: "Continuing after storage verification."})
				}}
				if manual {
					if _, err := r.Command("/compact"); err != nil {
						t.Fatal(err)
					}
					if !r.HasNotifications() {
						t.Fatal("manual handoff dropped recovered answer")
					}
				}
				if err := r.Run(nil); err != nil {
					t.Fatal(err)
				}
				if summaries != 1 || coding != 1 || r.HasNotifications() || delivered == 0 {
					t.Fatal("unexpected recovery", summaries, coding, delivered)
				}
				var acknowledged sql.NullInt64
				if err := r.Store.DB.QueryRow("SELECT delivered_request_id FROM entries WHERE id=?", finish).Scan(&acknowledged); err != nil || acknowledged.Valid {
					t.Fatal("recovery changed source acknowledgment", acknowledged, err)
				}
				if err := r.Store.DB.QueryRow("SELECT delivered_request_id FROM entries WHERE id=?", delivered).Scan(&acknowledged); err != nil || !acknowledged.Valid {
					t.Fatal("recovery did not acknowledge its fresh event", acknowledged, err)
				}
				if _, err := r.Command("/load " + r.Current()); err != nil || r.HasNotifications() {
					t.Fatal("delivered answer replayed on reload", err)
				}
			})
		}
	}
}

func TestRuntimeInitializationReportsRecoveryReadFailure(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	if runtime, err := New(context.Background(), r.Store, r.Workspace, r.Provider, r.CurrentSelection(), "missing", r.Skills, tool.WebSearchConfig{}, nil); err == nil || runtime != nil {
		t.Fatal("missing persisted session did not fail initialization", runtime, err)
	}
}

func TestLoadCompactionBoundaryBeforeSummaryAdmission(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Earlier task")
	source := r.Current()
	if err := r.queueNotification(`{"type":"job_exit","job_id":"completed_before_naming"}`); err != nil {
		t.Fatal(err)
	}
	// A process may stop while compaction waits for independent naming, before
	// any compaction inference request has been admitted.
	r.namingDone = make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := r.compactContext(ctx, "", r.CurrentSelection(), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	var count int
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM model_requests WHERE session_id=? AND purpose='compaction'", source).Scan(&count); err != nil || count != 0 {
		t.Fatal("fixture admitted summary inference", count, err)
	}
	// This fixture has no naming worker; avoid waiting on an unowned channel
	// when the runtime is switched or closed.
	r.namingDone = nil
	if _, err := r.Command("/load " + source); err != nil || !r.HasNotifications() {
		t.Fatal("early compaction boundary lost saved notifications", err)
	}
}
