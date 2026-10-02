package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"scicode/internal/provider"
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

func TestTurnEndDisplaysElapsedAndPreservesStatus(t *testing.T) {
	for _, status := range []string{"completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			r, events := runtimeFixture(t, nil)
			if status == "completed" {
				r.Provider = &provider.Script{Responses: []provider.ScriptResponse{{Text: "Done."}}}
			} else if status == "interrupted" {
				r.Provider = &childProvider{stream: func(context.Context, provider.Request, func(provider.StreamEvent) error) error {
					return context.Canceled
				}}
			}
			message := provider.Message{Role: "user", Content: "A short request"}
			err := r.Run(&message)
			if (err == nil) != (status == "completed") {
				t.Fatalf("unexpected turn error: %v", err)
			}
			var raw []byte
			if err := r.Store.DB.QueryRow("SELECT content_json FROM entries WHERE json_extract(content_json,'$.type')='turn_end'").Scan(&raw); err != nil {
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
			var emitted string
			for len(events) > 0 {
				if event := <-events; event.Kind == "status" && strings.HasPrefix(event.Text, "Turn ") {
					emitted = event.Text
				}
			}
			if emitted != end.Text {
				t.Fatalf("emitted status %q differs from persisted status %q", emitted, end.Text)
			}
			var storedStatus string
			if err := r.Store.DB.QueryRow("SELECT status FROM turns WHERE actor_id='main'").Scan(&storedStatus); err != nil || storedStatus != status {
				t.Fatalf("internal status changed: %q, %v", storedStatus, err)
			}
		})
	}
}
