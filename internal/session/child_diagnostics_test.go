package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"ttc/internal/jobs"
	"ttc/internal/tool"
)

// Exercise real capture, progress callbacks and native tool serialization without
// a provider. Even an accidentally enabled generic wake must not bypass the
// assignment-completion handoff.
func TestChildDiagnosticsRemainExplicitlyReadableNotAutomaticallyDelivered(t *testing.T) {
	for _, status := range []string{"completed", "failed", "cancelled"} {
		for _, background := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/background=%t", status, background), func(t *testing.T) {
				r, _ := runtimeFixture(t, nil)
				r.Emit = nil
				seedRuntime(t, r, "Observe child capture delivery")
				child := &codingChild{id: "main/child_test", state: "running"}
				assignment := &childAssignment{turn: "turn", background: background}
				notified := make(chan struct{})
				notify := r.Jobs.Notify
				r.Jobs.Notify = func(view jobs.Snapshot) {
					notify(view)
					close(notified)
				}
				ready, release := make(chan struct{}), make(chan struct{})
				id, err := r.Jobs.StartTask(child.id, jobs.TaskOptions{Kind: "subagent", Background: true, WakeOnExit: true}, func(ctx context.Context, stdout, stderr io.Writer) (jobs.TaskResult, error) {
					fmt.Fprint(stdout, "preview")
					fmt.Fprint(stderr, strings.Repeat("PRIVATE_STDERR_SENTINEL\n", 100))
					close(ready)
					select {
					case <-release:
					case <-ctx.Done():
						return jobs.TaskResult{}, ctx.Err()
					}
					if status == "failed" {
						return jobs.TaskResult{}, errors.New("actual failure")
					}
					return jobs.TaskResult{}, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				receive(t, ready)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				updates := 0
				final, err := r.Jobs.Wait(ctx, "main", id, func(view jobs.Snapshot) {
					updates++
					if view.Stderr == "" || !view.Truncated || view.StdoutTruncated {
						t.Fatal("fixture did not isolate suppressed stderr truncation", view)
					}
					object := requireNoChildDiagnostics(t, r.childResult(child, assignment, view))
					if string(object["truncated"]) != "false" {
						t.Fatal("stderr truncation affected child response", object)
					}
					if updates == 1 {
						if status == "cancelled" {
							if _, err := r.Jobs.Stop("main", id); err != nil {
								t.Fatal(err)
							}
						} else {
							close(release)
						}
					}
				})
				if err != nil || updates == 0 || final.Status != status {
					t.Fatal("capture lifecycle not exercised", final, updates, err)
				}
				// Join the notification callback, not just task completion.
				receive(t, notified)
				if r.HasNotifications() {
					t.Fatal("generic child job_exit bypassed canonical assignment handoff")
				}
				for _, request := range []struct{ name, args string }{
					{"job_list", `{"state":"all","details":true}`},
					{"job_stop", fmt.Sprintf(`{"job_id":%q}`, id)},
					{"job_read", fmt.Sprintf(`{"job_id":%q}`, id)},
				} {
					record := r.Tools.Invoke(context.Background(), tool.Execution{Actor: "main"}, request.name, []byte(request.args))
					var object map[string]json.RawMessage
					if err := json.Unmarshal(record.Result, &object); err != nil || string(object["ok"]) != "true" {
						t.Fatal(request.name, string(record.Result), err)
					}
					if strings.Contains(string(record.Result), "PRIVATE_STDERR_SENTINEL") || object["stderr_selection"] != nil {
						t.Fatal("unrequested stderr in native tool result", request.name, string(record.Result))
					}
				}
				// Model-facing explicit access still returns the retained stderr.
				read := r.Tools.Invoke(context.Background(), tool.Execution{Actor: "main"}, "job_read", []byte(fmt.Sprintf(`{"job_id":%q,"stream":"stderr","limit_bytes":65536}`, id)))
				if !strings.Contains(string(read.Result), "PRIVATE_STDERR_SENTINEL") || status == "failed" && !strings.Contains(string(read.Result), "actual failure") {
					t.Fatal("explicit diagnostic read lost captured evidence", string(read.Result))
				}
				// The execution inspector keeps both streams for humans.
				if !strings.Contains(r.JobDetail(id, ""), "PRIVATE_STDERR_SENTINEL") {
					t.Fatal("human inspector lost child diagnostic capture")
				}
			})
		}
	}
}
