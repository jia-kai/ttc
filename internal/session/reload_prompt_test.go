package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"scicode/internal/provider"
)

func TestReloadPromptAppendFailurePreservesActiveRuntime(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Earlier saved research")
	target := r.Current()
	id, err := r.Store.RecordSystemPrompt(target, "", "main", 0, "old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.DB.Exec("UPDATE entries SET created_ms=? WHERE id=?", time.Now().Add(-2*time.Hour).UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Command("/new"); err != nil {
		t.Fatal(err)
	}
	current, generation, jobs := r.Current(), r.Generation(), r.Jobs
	if _, err := r.Store.DB.Exec(`CREATE TRIGGER reject_prompt BEFORE INSERT ON entries WHEN json_extract(NEW.content_json,'$.type')='system_prompt' BEGIN SELECT RAISE(ABORT,'injected prompt failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Command("/load " + target); err == nil || !strings.Contains(err.Error(), "refresh instructions") {
		t.Fatal(err)
	}
	if r.Current() != current || r.Generation() != generation || r.Jobs != jobs {
		t.Fatal("failed load replaced the live runtime")
	}
}

func TestRefreshInstructionsUsesActiveBranch(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Earlier branch research")
	saved := r.Current()
	old, err := r.Store.RecordSystemPrompt(saved, "", "main", 0, "old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.DB.Exec("UPDATE entries SET created_ms=? WHERE id=?", time.Now().Add(-2*time.Hour).UnixMilli(), old); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.RecordSystemPrompt(saved, "", "main", 0, "undone recent prompt"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.DB.Exec("UPDATE sessions SET active_entry_id=? WHERE id=?", old, saved); err != nil {
		t.Fatal(err)
	}
	metadata, err := r.Store.Session(saved)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := RefreshInstructions(r.Store, metadata); err != nil || id == 0 {
		t.Fatal("off-branch prompt suppressed refresh", id, err)
	}
}

func TestAgedReloadPreservesRedo(t *testing.T) {
	r, _ := runtimeFixture(t, []provider.ScriptResponse{{Text: "remembered"}})
	seedRuntime(t, r, "Earlier research before undo")
	saved := r.Current()
	if _, err := r.Store.RecordSystemPrompt(saved, "", "main", 0, "old"); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(&provider.Message{Role: "user", Content: "remember this"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.DB.Exec("UPDATE entries SET created_ms=? WHERE session_id=? AND json_extract(content_json,'$.type')='system_prompt'", time.Now().Add(-2*time.Hour).UnixMilli(), saved); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Command("/undo"); err != nil {
		t.Fatal(err)
	}
	before, err := r.Store.Session(saved)
	if err != nil || before.RedoTip == 0 {
		t.Fatal(before, err)
	}
	if _, err := r.Command("/new"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Command("/load " + saved); err != nil {
		t.Fatal(err)
	}
	after, err := r.Store.Session(saved)
	if err != nil || after.RedoTip != before.RedoTip {
		t.Fatal("instruction refresh discarded redo", after, err)
	}
	if _, err := r.Command("/redo"); err != nil {
		t.Fatal(err)
	}
	redone, err := r.Store.Session(saved)
	if err != nil || redone.EntryTip != before.RedoTip {
		t.Fatal("redo did not restore selected history", redone, err)
	}
}

func TestReloadRefreshesAgedInstructionsForNextRequest(t *testing.T) {
	for _, age := range []time.Duration{59 * time.Minute, time.Hour, 2 * time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			seedRuntime(t, r, "Saved research to reload")
			saved := r.Current()
			id, err := r.Store.RecordSystemPrompt(saved, "", "main", 0, "obsolete instructions")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Store.DB.Exec("UPDATE entries SET created_ms=? WHERE id=?", time.Now().Add(-age).UnixMilli(), id); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Command("/new"); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Command("/load " + saved); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := r.Store.DB.QueryRow("SELECT count(*) FROM entries WHERE session_id=? AND json_extract(content_json,'$.type')='system_prompt'", saved).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 1
			if age >= time.Hour {
				want = 2
			}
			if count != want {
				t.Fatal(age, count, want)
			}
			r.Provider = &childProvider{stream: func(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
				if req.System != systemTemplate {
					t.Fatal("next request did not use current instructions")
				}
				return emit(provider.StreamEvent{Kind: "text", Text: "continuation"})
			}}
			if err := r.Run(&provider.Message{Role: "user", Content: "continue"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTimerCancelNameSkipsRetiredSameName(t *testing.T) {
	w := newWakeups(context.Background(), func(string) {})
	defer w.close()
	first, err := w.schedule("check", "old", time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.stop(first.ID, ""); err != nil {
		t.Fatal(err)
	}
	next, err := w.schedule("check", "current", time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := w.stop("", "check")
	if err != nil || stopped.ID != next.ID {
		t.Fatal(stopped, err)
	}
}
