package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"ttc/internal/llm"
	"ttc/internal/render"
)

func appendRecoveryRuntime(t *testing.T, s *Store, session, body string) int64 {
	t.Helper()
	id, err := s.Append(session, "", "main", "status", "", false, map[string]any{"type": "runtime_event", "body": json.RawMessage(body)})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func appendRecoveryChild(t *testing.T, s *Store, session string) int64 {
	t.Helper()
	turn, err := s.BeginChildTurn(session, "main/child", llm.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.FinishChildTurn(session, ChildFinish{ChildID: "main/child", TurnID: turn, JobID: "job_child", Status: "completed", Answer: "durable child answer"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func recoveryNotices(t *testing.T, s *Store, session string) []llm.Message {
	t.Helper()
	m, err := s.PendingRecoveryNotifications(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range m {
		if message.Role != "user" || !message.Runtime || message.EventSeq <= 0 {
			t.Fatal(message)
		}
		var body struct {
			Seq int64 `json:"event_seq"`
		}
		if err := json.Unmarshal([]byte(message.Content), &body); err != nil || body.Seq != message.EventSeq {
			t.Fatal(message, err)
		}
		entry, err := s.Entry(message.EventSeq)
		if err != nil || entry.Source != 0 || entry.Visible {
			t.Fatal(entry, err)
		}
		var event recoveryEvent
		if err := json.Unmarshal(entry.Content, &event); err != nil || event.RecoveredFrom <= 0 {
			t.Fatal(event, err)
		}
		if string(event.Body) != message.Content {
			t.Fatal("stored body differs", string(event.Body), message.Content)
		}
	}
	return m
}

func sourceRecoveryState(t *testing.T, s *Store, session string) string {
	t.Helper()
	var out strings.Builder
	for _, table := range []string{"sessions", "entries", "turns", "model_requests"} {
		key := "session_id"
		if table == "sessions" {
			key = "id"
		}
		rows, err := s.DB.Query("SELECT * FROM "+table+" WHERE "+key+"=? ORDER BY id", session)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(cols))
			dest := make([]any, len(cols))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(&out, table, values)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return out.String()
}

func TestLoadCompactionBoundaryOwnsFreshPendingEvents(t *testing.T) {
	for _, status := range []string{"running", "failed", "interrupted", "completed"} {
		t.Run(status, func(t *testing.T) {
			s, source, turn, _ := historyFixture(t)
			delivered := appendRecoveryRuntime(t, s, source.ID, `{"type":"job_exit","job_id":"delivered"}`)
			admitted, err := s.AdmitRequest(source.ID, turn, "main", source.Model, nil, []llm.Message{{Role: "user", Runtime: true, Content: `{"type":"job_exit"}`, EventSeq: delivered}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			child := appendRecoveryChild(t, s, source.ID)
			job := appendRecoveryRuntime(t, s, source.ID, `{"type":"job_exit","job_id":"pending"}`)
			request, err := s.StartRequest(source.ID, turn, "main", "compaction", source.Model)
			if err != nil {
				t.Fatal(err)
			}
			appendRecoveryRuntime(t, s, source.ID, `{"type":"wakeup","wakeup_id":"repeat","message":"old","fired_count":1}`)
			last := appendRecoveryRuntime(t, s, source.ID, `{"type":"wakeup","wakeup_id":"repeat","message":"latest","fired_count":2}`)
			if status != "running" {
				if err := s.FinishRequest(request, status, []any{}); err != nil {
					t.Fatal(err)
				}
			}
			before := sourceRecoveryState(t, s, source.ID)
			loaded, err := s.Load(source.ID)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.ID == source.ID || loaded.ReadOnly || loaded.FileTip != 0 {
				t.Fatal(loaded)
			}
			notices := recoveryNotices(t, s, loaded.ID)
			if len(notices) != 3 {
				t.Fatal(notices)
			}
			want := []int64{child, job, last}
			for i, m := range notices {
				entry, _ := s.Entry(m.EventSeq)
				var event recoveryEvent
				_ = json.Unmarshal(entry.Content, &event)
				if event.RecoveredFrom != want[i] || m.EventSeq == want[i] {
					t.Fatal(event, m)
				}
			}
			if !strings.Contains(notices[0].Content, "durable child answer") || !strings.Contains(notices[2].Content, "latest") {
				t.Fatal(notices)
			}
			entries, err := s.Branch(loaded.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, entry := range entries {
				if entry.EventSeq() == child {
					found = true
					if entry.Source != child {
						t.Fatal(entry)
					}
				}
			}
			if !found {
				t.Fatal("missing copied source chronology")
			}
			if got := sourceRecoveryState(t, s, source.ID); got != before {
				t.Fatal("load changed source", got, before)
			}
			// Repeated reads do not append or duplicate notifications.
			if again := recoveryNotices(t, s, loaded.ID); !reflect.DeepEqual(again, notices) {
				t.Fatal(again, notices)
			}
			reloaded, err := s.Load(loaded.ID)
			if err != nil {
				t.Fatal(err)
			}
			recoveredAgain := recoveryNotices(t, s, reloaded.ID)
			if len(recoveredAgain) != 3 {
				t.Fatal(recoveredAgain)
			}
			for i, m := range recoveredAgain {
				if m.EventSeq == notices[i].EventSeq {
					t.Fatal("reused original event")
				}
			}
			loadedTurn, err := s.BeginTurn(reloaded.ID, "async", reloaded.Model)
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := s.AdmitRequest(reloaded.ID, loadedTurn, "main", reloaded.Model, nil, recoveredAgain, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := recoveryNotices(t, s, reloaded.ID); len(got) != 0 {
				t.Fatal("admitted events remain pending", got)
			}
			if got := sourceRecoveryState(t, s, source.ID); got != before {
				t.Fatal("admission changed source")
			}
			var ack string
			if err := s.DB.QueryRow("SELECT delivered_events_json FROM model_requests WHERE id=?", fresh.RequestID).Scan(&ack); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(ack, fmt.Sprintf("[%d,", child)) {
				t.Fatal("acknowledged source", ack)
			}
			afterAdmission, err := s.Load(reloaded.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := recoveryNotices(t, s, afterAdmission.ID); len(got) != 0 {
				t.Fatal("replayed after admission", got)
			}
			var originalDelivery int64
			if err := s.DB.QueryRow("SELECT delivered_request_id FROM entries WHERE id=?", delivered).Scan(&originalDelivery); err != nil || originalDelivery != admitted.RequestID {
				t.Fatal(originalDelivery, err)
			}
		})
	}
}

func TestLoadRecoverySelectedBranchAndBoundaryOnly(t *testing.T) {
	for _, mode := range []string{"ordinary", "child_compaction", "abandoned_compaction", "abandoned_event", "coding_after_compaction", "fatal", "readonly"} {
		t.Run(mode, func(t *testing.T) {
			s, source, turn, _ := historyFixture(t)
			appendRecoveryRuntime(t, s, source.ID, `{"type":"job_exit","job_id":"selected"}`)
			selected, err := s.Session(source.ID)
			if err != nil {
				t.Fatal(err)
			}
			expect := 0
			switch mode {
			case "child_compaction":
				_, err = s.StartRequest(source.ID, "", "main/child", "compaction", source.Model)
			case "abandoned_compaction":
				_, err = s.StartRequest(source.ID, turn, "main", "compaction", source.Model)
				if err == nil {
					_, err = s.DB.Exec("UPDATE sessions SET active_entry_id=? WHERE id=?", selected.EntryTip, source.ID)
				}
			case "abandoned_event":
				_, err = s.StartRequest(source.ID, turn, "main", "compaction", source.Model)
				if err != nil {
					t.Fatal(err)
				}
				boundary, _ := s.Session(source.ID)
				appendRecoveryRuntime(t, s, source.ID, `{"type":"job_exit","job_id":"abandoned"}`)
				_, err = s.DB.Exec("UPDATE sessions SET active_entry_id=? WHERE id=?", boundary.EntryTip, source.ID)
				expect = 1
			case "coding_after_compaction":
				_, err = s.StartRequest(source.ID, turn, "main", "compaction", source.Model)
				if err == nil {
					_, err = s.StartRequest(source.ID, turn, "main", "coding", source.Model)
				}
			case "fatal":
				_, err = s.StartRequest(source.ID, turn, "main", "compaction", source.Model)
				if err == nil {
					err = s.InvalidateContext(source.ID, "does not fit")
				}
			case "readonly":
				_, err = s.StartRequest(source.ID, turn, "main", "compaction", source.Model)
				if err == nil {
					_, err = s.DB.Exec("UPDATE sessions SET read_only=1 WHERE id=?", source.ID)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before := sourceRecoveryState(t, s, source.ID)
			loaded, err := s.Load(source.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := recoveryNotices(t, s, loaded.ID); len(got) != expect {
				t.Fatal(got)
			}
			if mode == "readonly" && loaded.ID != source.ID {
				t.Fatal(loaded)
			}
			if mode == "fatal" && loaded.CompactionError == "" {
				t.Fatal("cleared fatal context")
			}
			if sourceRecoveryState(t, s, source.ID) != before {
				t.Fatal("changed source")
			}
		})
	}
}

func TestLoadRecoverySkipsChildFinishCarriedByVisibleTool(t *testing.T) {
	s, source, turn, request := historyFixture(t)
	finish := appendRecoveryChild(t, s, source.ID)
	_, calls, err := s.Assistant(source.ID, turn, "main", request, llm.Message{Role: "assistant", Calls: []llm.ToolCall{{ID: "child", Name: "subagent", Arguments: []byte(`{}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	result := []byte(fmt.Sprintf(`{"ok":true,"finish_event_seq":%d}`, finish))
	if _, err = s.CallResult(source.ID, turn, "main", calls[0], result, nil, map[string]string{}, render.Markdown{}, true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.StartRequest(source.ID, turn, "main", "compaction", source.Model); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := recoveryNotices(t, s, loaded.ID); len(got) != 0 {
		t.Fatal("duplicated tool context", got)
	}
	var ack sql.NullInt64
	if err := s.DB.QueryRow("SELECT delivered_request_id FROM entries WHERE id=?", finish).Scan(&ack); err != nil || ack.Valid {
		t.Fatal("mutated source ack", ack, err)
	}
}

func TestLoadRecoveryFindsSummarizedContinuationAncestors(t *testing.T) {
	s, source, turn, _ := historyFixture(t)
	finish := appendRecoveryChild(t, s, source.ID)
	if _, err := s.StartRequest(source.ID, turn, "main", "compaction", source.Model); err != nil {
		t.Fatal(err)
	}
	archive, err := s.ArchiveTranscript(source.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	tip, _ := s.Session(source.ID)
	first, err := s.Continue(source.ID, "summary one", archive, tip.EntryTip+1, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	appendRecoveryRuntime(t, s, first.ID, `{"type":"job_exit","job_id":"during_handoff"}`)
	archive, err = s.ArchiveTranscript(first.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	tip, _ = s.Session(first.ID)
	second, err := s.Continue(first.ID, "summary two", archive, tip.EntryTip+1, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	before := sourceRecoveryState(t, s, source.ID) + sourceRecoveryState(t, s, first.ID) + sourceRecoveryState(t, s, second.ID)
	loaded, err := s.Load(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := recoveryNotices(t, s, loaded.ID)
	if len(got) != 2 || !strings.Contains(got[0].Content, "durable child answer") {
		t.Fatal(got)
	}
	var event recoveryEvent
	entry, _ := s.Entry(got[0].EventSeq)
	_ = json.Unmarshal(entry.Content, &event)
	if event.RecoveredFrom != finish {
		t.Fatal(event)
	}
	if after := sourceRecoveryState(t, s, source.ID) + sourceRecoveryState(t, s, first.ID) + sourceRecoveryState(t, s, second.ID); after != before {
		t.Fatal("changed continuation ancestry")
	}
	// An in-memory runtime can compact before admitting recovered notices; the
	// read API must follow original ownership through that continuation as well.
	archive, err = s.ArchiveTranscript(loaded.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.Continue(loaded.ID, "recovered handoff", archive, loaded.EntryTip+1, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if inherited := recoveryNotices(t, s, next.ID); !reflect.DeepEqual(inherited, got) {
		t.Fatal(inherited, got)
	}
}

func TestLoadRecoveryRejectsCorruptBodiesAtomically(t *testing.T) {
	for _, body := range []string{`null`, `[]`, `42`, `{"type":7}`, `{}`, `{"type":"wakeup"}`} {
		t.Run(body, func(t *testing.T) {
			s, source, turn, _ := historyFixture(t)
			appendRecoveryRuntime(t, s, source.ID, body)
			if _, err := s.StartRequest(source.ID, turn, "main", "compaction", source.Model); err != nil {
				t.Fatal(err)
			}
			var before int
			if err := s.DB.QueryRow("SELECT count(*) FROM sessions").Scan(&before); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Load(source.ID); err == nil {
				t.Fatal("accepted damaged recovery body")
			}
			var after int
			if err := s.DB.QueryRow("SELECT count(*) FROM sessions").Scan(&after); err != nil || before != after {
				t.Fatal(before, after, err)
			}
		})
	}
}
