package session

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"ttc/internal/capture"
	"ttc/internal/history"
	"ttc/internal/jobs"
	"ttc/internal/llm"
)

func childAPIObject(t *testing.T, value any) map[string]json.RawMessage {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func requireNoChildDiagnostics(t *testing.T, value any) map[string]json.RawMessage {
	t.Helper()
	object := childAPIObject(t, value)
	for _, key := range []string{"stderr", "stderr_selection"} {
		if object[key] != nil {
			t.Fatalf("child diagnostics leaked through %s: %s", key, object[key])
		}
	}
	body, _ := json.Marshal(object)
	if strings.Contains(string(body), "PRIVATE_STDERR_SENTINEL") {
		t.Fatalf("child diagnostics leaked: %s", body)
	}
	return object
}

func TestChildToolResultSuppressesDiagnosticsAcrossLifecycle(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	for _, background := range []bool{false, true} {
		for _, status := range []string{"running", "completed", "failed", "cancelled"} {
			for _, stdoutTruncated := range []bool{false, true} {
				name := status + "/background=" + strconv.FormatBool(background) + "/stdout_truncated=" + strconv.FormatBool(stdoutTruncated)
				t.Run(name, func(t *testing.T) {
					child := &codingChild{id: "main/child_test", state: "running"}
					assignment := &childAssignment{turn: "turn", background: background}
					view := jobs.Snapshot{
						ID: "job", Kind: "subagent", Status: status, Stdout: "preview",
						Stderr: "PRIVATE_STDERR_SENTINEL", Truncated: true, StdoutTruncated: stdoutTruncated,
						StdoutSelection: &capture.Selection{TotalBytes: 7, HeadBytes: 7},
						StderrSelection: &capture.Selection{TotalBytes: 9999, OmittedBytes: 9000, CleaningError: "PRIVATE_STDERR_SENTINEL"},
					}
					if stdoutTruncated {
						view.StdoutSelection.OmittedBytes = 1
					}
					if status != "running" {
						assignment.completion = history.ChildFinish{
							ChildID: child.id, TurnID: "turn", JobID: "job", Outcome: status, ContextState: "closed",
							Answer: "partial answer", Truncated: true, Error: "actual cause", Warning: "actual warning",
							TranscriptPath: "evidence.md", TranscriptJSONLPath: "evidence.jsonl", TranscriptExportError: "actual export failure",
						}
					}
					object := requireNoChildDiagnostics(t, r.childResult(child, assignment, view))
					preview := !background && status == "running"
					if (object["stdout"] != nil) != preview || (object["stdout_selection"] != nil) != preview {
						t.Fatal("stdout selection did not follow delivered preview", object)
					}
					if string(object["truncated"]) != strconv.FormatBool(preview && stdoutTruncated) {
						t.Fatal("truncation included suppressed capture", object)
					}
					if status != "running" {
						notice := requireNoChildDiagnostics(t, childCompletionNotification(assignment.completion))
						if !background {
							notice = object
						}
						for _, key := range []string{"answer", "answer_truncated", "error", "warning", "transcript_path", "transcript_jsonl_path", "transcript_export_error"} {
							if notice[key] == nil {
								t.Fatalf("lost completion evidence %s: %v", key, notice)
							}
						}
					}
				})
			}
		}
	}
}

func TestChildCompletionStorageAndAPIContractsAreSeparate(t *testing.T) {
	const largeID int64 = 9007199254740993
	for _, state := range []string{"idle", "closed", "unknown"} {
		t.Run(state, func(t *testing.T) {
			record := history.ChildFinish{
				Type: "child_turn_finished", ChildID: "main/child_test", TurnID: "turn", JobID: "job",
				Outcome: "completed", ContextState: state, Persistent: true,
				ResultEntry: largeID, Answer: "Observed evidence", Truncated: true,
				Error: "cause", Warning: "warning", TranscriptPath: "transcript.md",
				TranscriptJSONLPath: "transcript.md.jsonl", TranscriptExportError: "export failure",
			}
			stored := childAPIObject(t, record)
			if stored["outcome"] == nil || stored["context_state"] == nil || stored["assignment_status"] != nil || stored["child_state"] != nil {
				t.Fatal("storage inherited the API contract", stored)
			}
			// Storage extensions stay durable data; they are not implicitly public.
			stored["private_extension"] = json.RawMessage(`{"precise":9007199254740993}`)
			stored["stderr"] = json.RawMessage(`"PRIVATE_STDERR_SENTINEL"`)
			stored["stderr_selection"] = json.RawMessage(`{"cleaning_error":"PRIVATE_STDERR_SENTINEL"}`)
			body, err := json.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			messages, err := recoveredNotificationMessages([]history.PendingNotification{{EventSeq: largeID + 2, RecoveredFrom: largeID + 1, Body: body}})
			if err != nil || len(messages) != 1 {
				t.Fatal(messages, err)
			}
			if messages[0].Type != llm.TypeRuntimeNotification || messages[0].EventSeq != largeID+2 {
				t.Fatal("notification identity lost", messages[0])
			}
			var recovered map[string]json.RawMessage
			if err := json.Unmarshal([]byte(messages[0].Content), &recovered); err != nil {
				t.Fatal(err)
			}
			if string(recovered["event_seq"]) != strconv.FormatInt(largeID+2, 10) || string(recovered["recovered_from_event_seq"]) != strconv.FormatInt(largeID+1, 10) || string(recovered["result_entry_id"]) != strconv.FormatInt(largeID, 10) {
				t.Fatal("routing or result identity lost precision", messages[0].Content)
			}
			delete(recovered, "event_seq")
			delete(recovered, "recovered_from_event_seq")
			live := requireNoChildDiagnostics(t, childCompletionNotification(record))
			requireNoChildDiagnostics(t, recovered)
			if !reflect.DeepEqual(live, recovered) {
				t.Fatal("live/recovered completion projections differ", live, recovered)
			}
			if live["outcome"] != nil || live["context_state"] != nil || live["private_extension"] != nil || live["status"] != nil {
				t.Fatal("storage fields leaked into API", live)
			}
			if state == "unknown" {
				if live["child_state"] != nil {
					t.Fatal("unknown historical state invented", live)
				}
			} else if string(live["child_state"]) != strconv.Quote(state) {
				t.Fatal("observed completion state lost", live)
			}
		})
	}
}

func TestChildToolResultUsesExplicitExecutionFields(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	child := &codingChild{id: "main/child_test", state: "running"}
	assignment := &childAssignment{turn: "turn", persistent: true}
	view := jobs.Snapshot{ID: "job", Kind: "subagent", Owner: child.id, Label: "audit", Status: "running", RecordDir: "must-not-leak", Stdout: "preview", Stderr: "diagnostic"}
	preview := childAPIObject(t, r.childResult(child, assignment, view))
	if string(preview["stdout"]) != `"preview"` || preview["record_dir"] != nil || preview["status"] != nil || preview["wake_on_exit"] != nil {
		t.Fatal("generic snapshot changed the child API", preview)
	}
	assignment.completion = history.ChildFinish{ChildID: child.id, TurnID: assignment.turn, JobID: view.ID, Outcome: "failed", ContextState: "closed", Answer: "partial", Error: "cause", Warning: "inspect evidence", ResultEntry: 42}
	assignment.finish = 43
	foreground := childAPIObject(t, r.childResult(child, assignment, view))
	if string(foreground["answer"]) != `"partial"` || string(foreground["finish_event_seq"]) != "43" || foreground["stdout"] != nil {
		t.Fatal("foreground completion lost handoff", foreground)
	}
	assignment.background = true
	background := childAPIObject(t, r.childResult(child, assignment, view))
	for _, key := range []string{"answer", "result_entry_id", "finish_event_seq", "error", "warning", "stdout", "transcript_path", "outcome", "context_state", "status"} {
		if background[key] != nil {
			t.Fatalf("background launch leaked %s: %s", key, background[key])
		}
	}
	if string(background["assignment_status"]) != `"failed"` || string(background["child_state"]) != `"closed"` {
		t.Fatal("fast background completion lost lifecycle", background)
	}
}

func TestRecoveredNonChildNotificationPreservesBodyAndPrecision(t *testing.T) {
	const original = `{"type":"job_exit","job_id":"job","detail":{"exact":9007199254740993},"event_seq":1,"recovered_from_event_seq":{"stale":2}}`
	body := json.RawMessage(original)
	messages, err := recoveredNotificationMessages([]history.PendingNotification{{EventSeq: 9, RecoveredFrom: 8, Body: body}})
	if err != nil || len(messages) != 1 {
		t.Fatal(messages, err)
	}
	var actual map[string]json.RawMessage
	if err := json.Unmarshal([]byte(messages[0].Content), &actual); err != nil {
		t.Fatal(err)
	}
	if string(actual["event_seq"]) != "9" || string(actual["recovered_from_event_seq"]) != "8" || string(actual["detail"]) != `{"exact":9007199254740993}` || string(body) != original {
		t.Fatal("non-child body changed or used untrusted routing", messages[0].Content)
	}
	if _, err := recoveredNotificationMessages([]history.PendingNotification{{EventSeq: 9, Body: json.RawMessage(`{`)}}); err == nil {
		t.Fatal("invalid recovered JSON accepted")
	}
}
