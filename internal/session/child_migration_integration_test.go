package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"ttc/internal/historytest"

	"ttc/internal/history"
	"ttc/internal/history/migrate"
	"ttc/internal/llm"
	"ttc/internal/tool"
	"ttc/internal/workspace"
)

func TestSchema7ChildCompletionMigrationThroughSessionAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, outcomeKey, state string
		nested                  bool
	}{
		{name: "original_direct_unknown", outcomeKey: "status"},
		{name: "original_nested_unknown", outcomeKey: "status", nested: true},
		{name: "intermediate_direct_idle", outcomeKey: "assignment_status", state: "idle"},
		{name: "intermediate_nested_closed", outcomeKey: "assignment_status", state: "closed", nested: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			seedRuntime(t, r, "Continue the historical assignment")
			source, root, workspaceRoot := r.Current(), r.Store.Root, r.Workspace.Root
			selection, catalog := r.CurrentSelection(), r.Skills
			const actor = "main/child_historical"
			const answer = "Exact historical answer.\nEvidence: π and a trailing newline.\n"
			turn, err := r.Store.BeginChildTurn(source, actor, selection)
			if err != nil {
				t.Fatal(err)
			}
			result, err := r.Store.Append(source, turn, actor, "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: answer})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Store.FinishTurn(turn, "completed"); err != nil {
				t.Fatal(err)
			}
			// Append the old canonical JSON without passing through current writers.
			legacy := map[string]any{
				"type": "child_turn_finished", "child_id": actor, "child_turn_id": turn,
				"job_id": "job_historical", tc.outcomeKey: "completed", "persistent": true,
				"result_entry_id": result, "answer": answer,
				"private_extension": json.RawMessage(`{"exact":9007199254740993}`),
			}
			if tc.state != "" {
				legacy["child_state"] = tc.state
			}
			raw, err := json.Marshal(legacy)
			if err != nil {
				t.Fatal(err)
			}
			original, err := r.Store.Append(source, turn, actor, "status", "", false, json.RawMessage(raw))
			if err != nil {
				t.Fatal(err)
			}
			finish := original
			if tc.nested {
				// A prior recovery supersedes the original; migration must preserve
				// both identities, and the next recovery must deliver only this one.
				finish, err = r.Store.Append(source, "", "main", "status", "", false, map[string]any{
					"type": "runtime_event", "body": json.RawMessage(raw),
					"recovered_from_event_seq": original,
					"envelope_extension":       json.RawMessage(`9007199254740993`),
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.Store.Append(source, "", "main", "status", "", false, json.RawMessage(`{"type":"compaction_started"}`)); err != nil {
				t.Fatal(err)
			}
			before := make(map[int64]history.Entry)
			for _, id := range []int64{original, finish} {
				entry, err := r.Store.Entry(id)
				if err != nil {
					t.Fatal(err)
				}
				before[id] = entry
			}

			// This is exclusively the fixture's temporary database. Stop workers
			// before changing its version, then release the lifetime migration lock.
			r.Close()
			// Reconstruct the schema-7 index and compaction boundaries, not just
			// its version, so later migrations build their indexes from scratch.
			if _, err := historytest.DB(t, r.Store).Exec(`DROP INDEX compaction_summary;
				DROP INDEX entry_created;
				DROP INDEX entry_prompt_recall;
				DROP INDEX session_list_workspace;
				CREATE INDEX session_list ON sessions(last_activity_ms DESC,id);
				ALTER TABLE compactions ADD COLUMN archive_path TEXT NOT NULL DEFAULT '';
				ALTER TABLE compactions ADD COLUMN archive_sha256 TEXT NOT NULL DEFAULT '';
				ALTER TABLE compactions ADD COLUMN archive_exact_sha256 TEXT NOT NULL DEFAULT '';
				PRAGMA user_version=7`); err != nil {
				t.Fatal(err)
			}
			if err := r.Store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err := history.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			var version int
			if err := historytest.DB(t, store).QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != migrate.CurrentVersion {
				t.Fatalf("automatic migration version=%d: %v", version, err)
			}
			state := tc.state
			if state == "" {
				state = "unknown"
			}
			for id, old := range before {
				entry, err := store.Entry(id)
				if err != nil {
					t.Fatal(err)
				}
				var object map[string]json.RawMessage
				if err := json.Unmarshal(entry.Content, &object); err != nil {
					t.Fatal(err)
				}
				if tc.nested && id == finish {
					if string(object["recovered_from_event_seq"]) != strconv.FormatInt(original, 10) || string(object["envelope_extension"]) != "9007199254740993" {
						t.Fatal("migration changed recovery envelope", string(entry.Content))
					}
					if err := json.Unmarshal(object["body"], &object); err != nil {
						t.Fatal(err)
					}
				}
				if string(object["outcome"]) != `"completed"` || string(object["context_state"]) != strconv.Quote(state) || object["status"] != nil || object["assignment_status"] != nil || object["child_state"] != nil {
					t.Fatal("source is not canonical storage JSON", string(entry.Content))
				}
				if string(object["answer"]) != strconv.Quote(answer) || string(object["result_entry_id"]) != strconv.FormatInt(result, 10) || string(object["private_extension"]) != `{"exact":9007199254740993}` {
					t.Fatal("migration changed historical evidence", string(entry.Content))
				}
				old.Content, entry.Content = nil, nil
				if !reflect.DeepEqual(old, entry) {
					t.Fatal("migration changed immutable entry identity", old, entry)
				}
			}

			loaded, err := store.Load(source)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := store.PendingRecoveryNotifications(context.Background(), loaded.ID)
			if err != nil || len(pending) != 1 {
				t.Fatal("expected exactly one recovered completion", pending, err)
			}
			event := pending[0]
			if event.EventSeq <= finish || event.RecoveredFrom != finish {
				t.Fatal("recovery did not own a fresh event", event)
			}
			var canonical history.ChildFinish
			if err := json.Unmarshal(event.Body, &canonical); err != nil || canonical.Outcome != "completed" || canonical.ContextState != state || canonical.Answer != answer || canonical.ResultEntry != result {
				t.Fatal("load lost canonical completion", canonical, err)
			}
			w, err := workspace.Open(workspaceRoot, store)
			if err != nil {
				t.Fatal(err)
			}
			restarted, err := New(context.Background(), store, w, r.Provider, selection, loaded.ID, catalog, tool.WebSearchConfig{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(restarted.Close)
			restarted.AutoName = false
			assertAPI := func(message llm.Message) {
				t.Helper()
				var object map[string]json.RawMessage
				if err := json.Unmarshal([]byte(message.Content), &object); err != nil {
					t.Fatal(err)
				}
				var notice childNotification
				if err := json.Unmarshal([]byte(message.Content), &notice); err != nil {
					t.Fatal(err)
				}
				if message.Type != llm.TypeRuntimeNotification || message.EventSeq != event.EventSeq || notice.Type != "child_turn_finished" || notice.ChildID != actor || notice.TurnID != turn || notice.JobID != "job_historical" || notice.AssignmentStatus != "completed" || !notice.Persistent || notice.Answer == nil || *notice.Answer != answer || notice.ResultEntry == nil || *notice.ResultEntry != result {
					t.Fatal("API lost completion or routing identity", message)
				}
				if string(object["event_seq"]) != strconv.FormatInt(event.EventSeq, 10) || string(object["recovered_from_event_seq"]) != strconv.FormatInt(finish, 10) {
					t.Fatal("API lost recovery envelope", message.Content)
				}
				for _, key := range []string{"outcome", "context_state", "status", "private_extension"} {
					if object[key] != nil {
						t.Fatalf("storage field %s leaked into API: %s", key, message.Content)
					}
				}
				if tc.state == "" {
					if object["child_state"] != nil {
						t.Fatal("API invented missing historical state", message.Content)
					}
				} else if notice.ChildState != tc.state {
					t.Fatal("API lost observed child state", message.Content)
				}
			}
			if len(restarted.notifications) != 1 {
				t.Fatal("New did not project recovered notifications", restarted.notifications)
			}
			assertAPI(restarted.notifications[0])
			calls := 0
			restarted.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				calls++
				found := 0
				for _, message := range req.Messages {
					if message.Type == llm.TypeRuntimeNotification && message.EventSeq == event.EventSeq {
						assertAPI(message)
						found++
					}
				}
				if found != 1 {
					return fmt.Errorf("admitted completion count = %d, want 1", found)
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Historical evidence received."})
			}}
			if err := restarted.Run(nil); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || restarted.HasNotifications() {
				t.Fatal("completion was not consumed once", calls)
			}
			for _, id := range []int64{original, finish, event.EventSeq} {
				var delivered sql.NullInt64
				if err := historytest.DB(t, store).QueryRow("SELECT delivered_request_id FROM entries WHERE id=?", id).Scan(&delivered); err != nil || delivered.Valid != (id == event.EventSeq) {
					t.Fatal("delivery changed source ownership", id, delivered, err)
				}
			}
			if _, err := restarted.Command("/load " + restarted.Current()); err != nil || restarted.HasNotifications() {
				t.Fatal("delivered completion replayed after Load", err)
			}
		})
	}
}
