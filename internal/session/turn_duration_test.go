package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"ttc/internal/historytest"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
)

func TestTurnDuration(t *testing.T) {
	for _, test := range []struct {
		elapsed time.Duration
		want    string
	}{
		{-time.Second, "0s"},
		{0, "0s"},
		{time.Second - time.Nanosecond, "0s"},
		{time.Second, "1s"},
		{time.Minute - time.Nanosecond, "59s"},
		{time.Minute, "1min"},
		{time.Minute + time.Second, "1min1s"},
		{5*time.Minute + 32*time.Second + 999*time.Millisecond, "5min32s"},
		{time.Hour - time.Nanosecond, "59min59s"},
		{time.Hour, "1h"},
		{time.Hour + time.Second, "1h1s"},
		{24*time.Hour - time.Nanosecond, "23h59min59s"},
		{24 * time.Hour, "1d"},
		{49*time.Hour + 2*time.Minute + 3*time.Second, "2d1h2min3s"},
	} {
		t.Run(test.elapsed.String(), func(t *testing.T) {
			if got := turnDuration(test.elapsed); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}

func TestTurnCompletionRequiresAdmissionAndReleasesInput(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	if completion, err := r.Run(nil); completion != nil || err == nil {
		t.Fatalf("empty run reported an admitted turn: completion=%+v err=%v", completion, err)
	}
	input := r.PrepareInput(contextbuild.Input{Text: "Cancelled input"})
	if _, err := input.Cancel(); err != nil {
		t.Fatal(err)
	}
	if completion, err := r.RunInput(input); completion != nil || err != nil {
		t.Fatalf("cancelled admission reported a turn: completion=%+v err=%v", completion, err)
	}
	r.Provider = &llm.Script{Responses: []llm.ScriptResponse{{Text: "Done."}}}
	input = r.PrepareInput(contextbuild.Input{Text: "Admitted input"})
	completion, err := r.RunInput(input)
	if err != nil || completion == nil {
		t.Fatalf("admitted turn did not return completion: %+v %v", completion, err)
	}
	input.mu.Lock()
	retained := input.pending || input.input.Text != "" || len(input.input.Attachments) != 0
	input.mu.Unlock()
	if retained {
		t.Fatal("completion preceded input ticket cleanup")
	}
}

func TestTurnEndDisplaysElapsedAndPreservesStatus(t *testing.T) {
	for _, status := range []string{"completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			r, events := runtimeFixture(t, nil)
			if status == "completed" {
				r.Provider = &llm.Script{Responses: []llm.ScriptResponse{{Text: "Done."}}}
			} else if status == "interrupted" {
				r.Provider = &childProvider{stream: func(context.Context, llm.Request, func(llm.StreamEvent) error) error {
					return context.Canceled
				}}
			}
			message := llm.Message{Type: llm.TypeUserInput, Content: "A short request"}
			completion, err := r.Run(&message)
			if (err == nil) != (status == "completed") {
				t.Fatalf("unexpected turn error: %v", err)
			}
			var raw []byte
			if err := historytest.DB(t, r.Store).QueryRow("SELECT content_json FROM entries WHERE json_extract(content_json,'$.type')='turn_end'").Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var end struct {
				Status string `json:"status"`
				Text   string `json:"text"`
				WallMS int64  `json:"wall_ms"`
			}
			if err := json.Unmarshal(raw, &end); err != nil {
				t.Fatal(err)
			}
			displayStatus := status
			if status == "completed" {
				displayStatus = "complete"
			}
			prefix := "Turn " + displayStatus + " · " + turnDuration(time.Duration(end.WallMS)*time.Millisecond) + " · avg "
			if end.Status != status || end.WallMS < 0 || !strings.HasPrefix(end.Text, prefix) {
				t.Fatalf("wrong terminal status or timing: %+v", end)
			}
			if completion == nil || completion.Text != end.Text || completion.TurnID == "" || completion.EntryID <= 0 || completion.Generation != r.Generation() || completion.SessionID != r.Current() {
				t.Fatalf("returned completion differs from saved status: %+v", completion)
			}
			if !r.runMu.TryLock() {
				t.Fatal("completion returned before releasing the foreground lock")
			}
			r.runMu.Unlock()
			r.mu.Lock()
			active := r.activeCancel != nil || r.activeTurn != ""
			r.mu.Unlock()
			if active {
				t.Fatal("completion returned before clearing foreground ownership")
			}
			for len(events) > 0 {
				if event := <-events; event.Kind == "status" && strings.HasPrefix(event.Text, "Turn ") {
					t.Fatalf("terminal status bypassed the settled result: %+v", event)
				}
			}
			var storedStatus string
			if err := historytest.DB(t, r.Store).QueryRow("SELECT status FROM turns WHERE actor_id='main'").Scan(&storedStatus); err != nil || storedStatus != status {
				t.Fatalf("internal status changed: %q, %v", storedStatus, err)
			}
		})
	}
}
