package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"ttc/internal/tool"
)

func TestBackgroundCompletionDeliversSelectedOutputAndExitCode(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Observe the background command")
	record := r.Tools.Invoke(context.Background(), tool.Execution{Actor: "main"}, "shell", []byte(`{"command":"printf 'begin-middle-end'; printf 'error' >&2; exit 7","background":true,"output_mode":"head_tail","output_bytes":10}`))
	var launch struct {
		OK bool   `json:"ok"`
		ID string `json:"job_id"`
	}
	if err := json.Unmarshal(record.Result, &launch); err != nil || !launch.OK {
		t.Fatal(string(record.Result), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	final, err := r.Jobs.Wait(ctx, "main", launch.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	var content string
	for content == "" {
		r.orderMu.Lock()
		for _, notice := range r.notifications {
			var body struct {
				ID string `json:"job_id"`
			}
			if json.Unmarshal([]byte(notice.Content), &body) == nil && body.ID == launch.ID {
				content = notice.Content
			}
		}
		r.orderMu.Unlock()
		if content != "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("completion notification missing")
		case <-time.After(time.Millisecond):
		}
	}
	var body struct {
		Type            string `json:"type"`
		Stdout          string `json:"stdout"`
		Stderr          string `json:"stderr"`
		ExitCode        int    `json:"exit_code"`
		Truncated       bool   `json:"truncated"`
		StdoutSelection struct {
			Omitted int `json:"omitted_bytes"`
		} `json:"stdout_selection"`
	}
	if err := json.Unmarshal([]byte(content), &body); err != nil {
		t.Fatal(err)
	}
	if body.Type != "job_exit" || body.Stdout != final.Stdout || body.Stderr != final.Stderr || body.ExitCode != 7 || !body.Truncated || body.StdoutSelection.Omitted == 0 {
		t.Fatalf("completion did not deliver exact selected outcome: %s", content)
	}
}
