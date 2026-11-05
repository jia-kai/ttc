package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/llm"
	"ttc/internal/render"
)

type legacyForkCounts struct {
	sessions, entries, records, calls, turns, requests, observations, files int
}

func readLegacyForkCounts(t *testing.T, db *sql.DB) legacyForkCounts {
	t.Helper()
	var counts legacyForkCounts
	err := db.QueryRow(`SELECT
		(SELECT count(*) FROM sessions), (SELECT count(*) FROM entries),
		(SELECT count(*) FROM tool_records), (SELECT count(*) FROM tool_calls),
		(SELECT count(*) FROM turns), (SELECT count(*) FROM model_requests),
		(SELECT count(*) FROM request_observations), (SELECT count(*) FROM file_changes)`).Scan(
		&counts.sessions, &counts.entries, &counts.records, &counts.calls,
		&counts.turns, &counts.requests, &counts.observations, &counts.files)
	if err != nil {
		t.Fatal(err)
	}
	return counts
}

type legacyForkRecord struct {
	entry                 int64
	call                  string
	version               int
	content, presentation string
}

func readLegacyForkRecords(t *testing.T, db *sql.DB) []legacyForkRecord {
	t.Helper()
	rows, err := db.Query("SELECT entry_id,call_id,version,record_json,markdown_json FROM tool_records ORDER BY entry_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var records []legacyForkRecord
	for rows.Next() {
		var record legacyForkRecord
		if err := rows.Scan(&record.entry, &record.call, &record.version, &record.content, &record.presentation); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

// insertLegacyManualCopy is fixture SQL, not a supported alternative Load path.
// Schema 13 copied each selected envelope/record, chained fresh physical IDs,
// preserved original source/turn attribution, and hid copied runtime input.
func insertLegacyManualCopy(t *testing.T, s *Store, source Session, entries []Entry) (Session, map[int64]int64) {
	t.Helper()
	legacyID := NewID("legacy_manual")
	copies := make(map[int64]int64, len(entries))
	err := s.transact(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO sessions(id,workspace_id,lineage_id,name,name_source,model_json,last_activity_ms,metadata_json)
			SELECT ?,workspace_id,lineage_id,name,'manual',model_json,last_activity_ms,'{}' FROM sessions WHERE id=?`, legacyID, source.ID); err != nil {
			return err
		}
		var parent int64
		for _, entry := range entries {
			visible := entry.Visible
			if entry.Kind == "message" {
				var message llm.Message
				if err := json.Unmarshal(entry.Content, &message); err != nil {
					return err
				}
				if message.Type.IsRuntime() {
					visible = false
				}
			}
			inserted, err := tx.Exec(`INSERT INTO entries(session_id,parent_id,source_id,turn_id,main_turn_id,undo_owner_turn_id,actor_id,kind,role,model_visible,content_json,created_ms)
				SELECT ?,?,coalesce(e.source_id,e.id),e.turn_id,origin.main_turn_id,origin.undo_owner_turn_id,e.actor_id,e.kind,e.role,?,e.content_json,e.created_ms
				FROM entries e JOIN entries origin ON origin.id=coalesce(e.source_id,e.id) WHERE e.id=?`, legacyID, n(parent), visible, entry.ID)
			if err != nil {
				return err
			}
			parent, err = inserted.LastInsertId()
			if err != nil {
				return err
			}
			copies[entry.ID] = parent
			if entry.Kind == "tool_result" {
				if _, err := tx.Exec(`INSERT INTO tool_records(entry_id,call_id,version,record_json,markdown_json)
					SELECT ?,call_id,version,record_json,markdown_json FROM tool_records WHERE entry_id=?`, parent, entry.ID); err != nil {
					return err
				}
			}
		}
		_, err := tx.Exec("UPDATE sessions SET active_entry_id=?,undo_floor_id=? WHERE id=?", n(parent), n(parent), legacyID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := s.Session(legacyID)
	if err != nil {
		t.Fatal(err)
	}
	return legacy, copies
}

func TestSchema13LegacyManualCopyMigratesAndForksWithoutHistoricalCopies(t *testing.T) {
	s, source, turn := usageFixture(t)
	ctx := context.Background()
	admitted := admitUsage(t, s, source, turn, false)
	const system = "Original schema-13 measurement instructions.\n"
	if _, err := s.RecordSystemPrompt(source.ID, turn, "main", admitted.RequestID, system); err != nil {
		t.Fatal(err)
	}
	_, calls, err := s.Assistant(source.ID, turn, "main", admitted.RequestID, llm.Message{Type: llm.TypeAssistant, Calls: []llm.ToolCall{
		{ID: "legacy_read", Name: "read", Arguments: json.RawMessage(`{"path":"legacy.txt"}`)},
	}})
	if err != nil || len(calls) != 1 {
		t.Fatal(calls, err)
	}
	if _, err := s.CommitChange(source.ID, calls[0], []string{"synthetic-legacy-path"}, true); err != nil {
		t.Fatal(err)
	}
	md := render.Markdown{Revision: 1, Summary: "Legacy read result", Detail: "Original schema-13 tool inspection evidence"}
	result, err := s.CallResult(source.ID, turn, "main", calls[0], json.RawMessage(`{"ok":true,"content":"legacy tool evidence"}`), nil,
		map[string]string{"record": "original legacy record"}, md, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(source.ID, turn, "main", llm.Message{Type: llm.TypeAssistant, Content: "Legacy source response evidence"}); err != nil {
		t.Fatal(err)
	}
	runtime, err := s.AppendMessage(source.ID, turn, "main", llm.Message{Type: llm.TypeRuntimeInstruction, Content: "Original runtime remains inspectable"})
	if err != nil {
		t.Fatal(err)
	}
	finishUsage(t, s, admitted.RequestID, "completed", &llm.Usage{InputTokens: 210, OutputTokens: 12})
	if err := s.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	source, err = s.Session(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	sourceEntries, err := s.Branch(source.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	sourceContext, err := s.ReadContextState(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacy, mapping := insertLegacyManualCopy(t, s, source, sourceEntries)
	legacyEntries, err := s.Branch(legacy.ID, 0)
	if err != nil || len(legacyEntries) != len(sourceEntries) {
		t.Fatal("legacy fixture lacks physical copied rows", err)
	}
	var parent int64
	for i, copied := range legacyEntries {
		original := sourceEntries[i]
		visible := original.Visible
		if original.Kind == "message" {
			var message llm.Message
			if err := json.Unmarshal(original.Content, &message); err != nil {
				t.Fatal(err)
			}
			visible = visible && !message.Type.IsRuntime()
		}
		if copied.ID == original.ID || copied.ID != mapping[original.ID] || copied.Source != original.ID || copied.Parent != parent || copied.SessionID != legacy.ID ||
			copied.FileTip != 0 || copied.Visible != visible || copied.CreatedMS != original.CreatedMS || copied.TurnID != original.TurnID ||
			copied.MainTurnID != original.MainTurnID || copied.UndoOwnerTurnID != original.UndoOwnerTurnID || string(copied.Content) != string(original.Content) {
			t.Fatalf("fixture is not a genuine schema-13 manual copy: original=%+v copied=%+v", original, copied)
		}
		parent = copied.ID
	}
	if legacy.EntryTip == source.EntryTip || legacy.UndoFloor != legacy.EntryTip || legacy.FileTip != 0 || source.FileTip == 0 {
		t.Fatal("legacy fixture lost its independent copied baseline", source, legacy)
	}
	records := readLegacyForkRecords(t, s.db)
	if len(records) != 2 || records[1].entry != mapping[result] || records[1].call != records[0].call || records[1].content != records[0].content || records[1].presentation != records[0].presentation {
		t.Fatal("legacy fixture lacks a copied tool record", records)
	}
	counts := readLegacyForkCounts(t, s.db)
	root := s.Root
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Recreate the actual prior schema, not a version-13 stamp over schema 14.
	// Only this disposable fixture is opened without Store migration here.
	db, err := openDatabase(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE sessions DROP COLUMN base_entry_id;
		ALTER TABLE sessions DROP COLUMN base_session_id;
		PRAGMA user_version=13;`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	var version, baseColumns int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 13 {
		db.Close()
		t.Fatal("fixture is not schema 13", version, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM pragma_table_info('sessions') WHERE name IN ('base_session_id','base_entry_id')").Scan(&baseColumns); err != nil || baseColumns != 0 {
		db.Close()
		t.Fatal("fixture still has fork columns", baseColumns, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	if err := migrated.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatal("legacy fixture did not migrate", version, err)
	}
	if got := readLegacyForkCounts(t, migrated.db); got != counts {
		t.Fatal("migration rewrote/deduplicated legacy storage", got, counts)
	}
	assertOriginalRows := func() {
		t.Helper()
		for _, expected := range []struct {
			session Session
			entries []Entry
		}{{source, sourceEntries}, {legacy, legacyEntries}} {
			got, err := migrated.Branch(expected.session.ID, 0)
			if err != nil || !reflect.DeepEqual(got, expected.entries) {
				t.Fatal("migration/fork changed original content, IDs, timestamps, visibility or ownership", expected.session.ID, err)
			}
			metadata, err := migrated.Session(expected.session.ID)
			if err != nil || !reflect.DeepEqual(metadata, expected.session) {
				t.Fatal("migration/fork changed legacy session cursors or ownership", metadata, expected.session, err)
			}
		}
		var turnOwner, requestOwner, callOwner string
		if err := migrated.db.QueryRow(`SELECT t.session_id,r.session_id,c.session_id FROM turns t
			JOIN model_requests r ON r.turn_id=t.id JOIN tool_calls c ON c.request_id=r.id
			WHERE t.id=? AND r.id=? AND c.id=?`, turn, admitted.RequestID, calls[0]).Scan(&turnOwner, &requestOwner, &callOwner); err != nil ||
			turnOwner != source.ID || requestOwner != source.ID || callOwner != source.ID {
			t.Fatal("migration/fork moved original turn, request or tool ownership", turnOwner, requestOwner, callOwner, err)
		}
		if got := readLegacyForkRecords(t, migrated.db); !reflect.DeepEqual(got, records) {
			t.Fatal("migration/fork changed physical copied tool records", got, records)
		}
	}
	assertOriginalRows()
	var inferred int
	if err := migrated.db.QueryRow("SELECT count(*) FROM sessions WHERE base_session_id IS NOT NULL OR base_entry_id IS NOT NULL").Scan(&inferred); err != nil || inferred != 0 {
		t.Fatal("migration inferred/deduplicated legacy forks", inferred, err)
	}
	inspection, err := migrated.ReadInspection(ctx, mapping[result])
	if err != nil || inspection.Text != md.Detail {
		t.Fatal("migrated copied tool record is not inspectable", inspection, err)
	}
	canonical, err := migrated.ReadCommittedMessage(ctx, mapping[runtime.EntryID])
	if err != nil || canonical.CreatedMS != runtime.CreatedMS || !reflect.DeepEqual(canonical.Message, runtime.Message) {
		t.Fatal("migrated hidden runtime copy lost canonical evidence", canonical, err)
	}
	projected, err := migrated.ReadContextState(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	var expectedInput []llm.Message
	for _, message := range sourceContext.Messages {
		if !message.Type.IsRuntime() {
			expectedInput = append(expectedInput, message)
		}
	}
	if !reflect.DeepEqual(projected.Messages, expectedInput) {
		t.Fatal("migrated manual copy changed its frozen main projection", projected.Messages, expectedInput)
	}
	usage := projected.Usage
	if usage.Reported == nil || usage.Anchor == nil || usage.Reported.RequestID != admitted.RequestID || usage.Anchor.RequestID != admitted.RequestID ||
		usage.Anchor.Checkpoint != nil || usage.Anchor.System != system || !reflect.DeepEqual(usage.Anchor.Messages, admitted.Messages) || usage.Reported.Tokens.InputTokens != 210 {
		t.Fatal("migrated manual copy lost original source-backed usage input", usage)
	}
	conversation, err := migrated.Transcript(legacy.ID, 0)
	if err != nil || !strings.Contains(string(conversation), "Legacy source response evidence") {
		t.Fatal("migrated manual copy export failed", err)
	}
	exact, err := migrated.ActorTranscriptJSONL(legacy.ID, "main", legacy.EntryTip)
	if err != nil || !strings.Contains(string(exact), "Legacy source response evidence") || !strings.Contains(string(exact), runtime.Message.Content) {
		t.Fatal("migrated copied actor export failed", err)
	}
	var sourceEvents []int64
	for _, entry := range sourceEntries {
		sourceEvents = append(sourceEvents, entry.ID)
	}
	if exported := transcriptEventIDs(t, exact); !reflect.DeepEqual(exported, sourceEvents) {
		t.Fatal("legacy actor export lost/doubled original source identities", exported, sourceEvents)
	}
	replay, err := migrated.ReadReplay(ctx, legacy.ID, 0)
	if err != nil || len(replay.Rows) == 0 {
		t.Fatal("migrated manual copy replay failed", err)
	}
	shared, err := migrated.Load(legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	counts.sessions++
	if got := readLegacyForkCounts(t, migrated.db); got != counts || shared.EntryTip != legacy.EntryTip || shared.UndoFloor != legacy.EntryTip {
		t.Fatal("fork of legacy manual copy copied/deduplicated historical storage", got, counts, shared)
	}
	var baseSession string
	var baseEntry int64
	if err := migrated.db.QueryRow("SELECT base_session_id,base_entry_id FROM sessions WHERE id=?", shared.ID).Scan(&baseSession, &baseEntry); err != nil || baseSession != legacy.ID || baseEntry != legacy.EntryTip {
		t.Fatal("new fork skipped its logical legacy source", baseSession, baseEntry, err)
	}
	sharedContext, err := migrated.ReadContextState(ctx, shared.ID)
	if err != nil || !reflect.DeepEqual(sharedContext, projected) {
		t.Fatal("fork of migrated legacy copy changed input/accounting", err)
	}
	sharedReplay, err := migrated.ReadReplay(ctx, shared.ID, 0)
	if err != nil || !reflect.DeepEqual(sharedReplay, replay) {
		t.Fatal("fork of migrated legacy copy changed replay", err)
	}
	sharedExact, err := migrated.ActorTranscriptJSONL(shared.ID, "main", shared.EntryTip)
	if err != nil || string(sharedExact) != string(exact) {
		t.Fatal("fork of migrated legacy copy changed source-backed actor export", err)
	}
	assertOriginalRows()
}
