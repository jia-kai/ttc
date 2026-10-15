package migrate

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func transaction(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	return tx
}

func checkProgress(t *testing.T, events []Progress, from int) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("missing progress")
	}
	for version := from; version < CurrentVersion; version++ {
		var step []Progress
		for _, event := range events {
			if event.FromVersion == version {
				step = append(step, event)
			}
		}
		if len(step) < 2 || step[0].Completed != 0 || step[0].Total <= 0 {
			t.Fatalf("schema %d lacks initial progress: %+v", version, step)
		}
		last := int64(0)
		for i, event := range step {
			if event.ToVersion != version+1 || event.Total != step[0].Total || event.Completed < last || event.Completed > event.Total || event.Phase == "" {
				t.Fatalf("invalid progress at %d: %+v", i, step)
			}
			if event.Phase == "complete" && i != len(step)-1 {
				t.Fatalf("premature completion: %+v", step)
			}
			last = event.Completed
		}
		end := step[len(step)-1]
		if end.Phase != "complete" || end.Completed != end.Total {
			t.Fatalf("schema %d lacks completion: %+v", version, step)
		}
	}
	version := from
	for _, event := range events {
		if event.FromVersion != version {
			t.Fatalf("out-of-order progress: %+v", events)
		}
		if event.Phase == "complete" {
			version++
		}
	}
	if version != CurrentVersion {
		t.Fatalf("incomplete chain: %+v", events)
	}
}

func TestApplyOrderedChainAndVersionBoundaries(t *testing.T) {
	db := fixture(t)
	tx := transaction(t, db)
	var events []Progress
	err := Apply(tx, func(event Progress) {
		events = append(events, event)
		if event.Completed == 0 || event.Phase == "complete" {
			var version int
			if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
				t.Fatal(err)
			}
			want := event.FromVersion
			if event.Phase == "complete" {
				want = event.ToVersion
			}
			if version != want {
				t.Fatalf("event %+v at schema version %d, want %d", event, version, want)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	checkProgress(t, events, 5)
	// The retained-input rewrite must precede semantic payload conversion.
	var retained, typ string
	if err := tx.QueryRow(`SELECT json_extract(content_json,'$.retained_at_ms'),json_extract(content_json,'$.type') FROM entries WHERE id=12`).Scan(&retained, &typ); err != nil {
		t.Fatal(err)
	}
	if retained != "200" || typ != "user_input" {
		t.Fatalf("chain omitted or reordered a step: retained=%q type=%q", retained, typ)
	}
	// Apply completes steps, but must leave ownership of the outer commit intact.
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := scalar(t, db, "PRAGMA user_version"); got != "5" {
		t.Fatal("Apply committed its transaction", got)
	}
}

func TestApplyStartsAtSchema6(t *testing.T) {
	db := fixture(t)
	applySchema5(t, db)
	tx := transaction(t, db)
	var events []Progress
	if err := Apply(tx, func(event Progress) { events = append(events, event) }); err != nil {
		t.Fatal(err)
	}
	checkProgress(t, events, 6)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := scalar(t, db, "PRAGMA user_version"); got != "7" {
		t.Fatal(got)
	}
}

func TestApplyCurrentVersionIsNoOp(t *testing.T) {
	// No history tables: a current database must not even enter an upgrade.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec(t, db, fmt.Sprintf("PRAGMA user_version=%d", CurrentVersion))
	tx := transaction(t, db)
	if err := Apply(tx, func(event Progress) { t.Fatalf("no-op reported progress: %+v", event) }); err != nil {
		t.Fatal(err)
	}
}

func TestApplyLaterStepFailureRollsBackWholeChain(t *testing.T) {
	db := fixture(t)
	for i := 0; i < messageTypeBatchSize+6; i++ {
		insert(t, db, int64(100+i), "old", 2, 0, "", "message", "assistant", `{"role":"assistant","content":"batch"}`, 150)
	}
	exec(t, db, `CREATE TRIGGER reject_semantic_update BEFORE UPDATE ON entries
		WHEN OLD.id=169 AND json_extract(NEW.content_json,'$.type')='assistant'
		BEGIN SELECT RAISE(ABORT,'later-step write failure'); END`)
	before := scalar(t, db, "SELECT json_group_array(json_array(id,kind,role,content_json)) FROM entries")
	cursors := scalar(t, db, "SELECT json_group_array(json_array(id,active_entry_id,redo_entry_id,undo_floor_id)) FROM sessions")
	tx := transaction(t, db)
	var events []Progress
	err := Apply(tx, func(event Progress) { events = append(events, event) })
	if err == nil || !strings.Contains(err.Error(), "migrate history schema 6 to 7") || !strings.Contains(err.Error(), "later-step write failure") {
		t.Fatal("missing later-step failure", err)
	}
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 6 {
		t.Fatalf("successful first step did not update transaction version: %d", version)
	}
	firstComplete, partialSecond := false, false
	for _, event := range events {
		if event.FromVersion == 5 && event.Phase == "complete" {
			firstComplete = true
		}
		if event.FromVersion == 6 {
			partialSecond = partialSecond || event.Completed >= messageTypeBatchSize
			if event.Phase == "complete" {
				t.Fatalf("failed step claimed completion: %+v", events)
			}
		}
	}
	if !firstComplete || !partialSecond {
		t.Fatalf("did not exercise partial later-step work: %+v", events)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := scalar(t, db, "PRAGMA user_version"); got != "5" {
		t.Fatal("rollback retained intermediate version", got)
	}
	if got := scalar(t, db, "SELECT json_group_array(json_array(id,kind,role,content_json)) FROM entries"); got != before {
		t.Fatal("rollback retained changes from one of the steps")
	}
	if got := scalar(t, db, "SELECT json_group_array(json_array(id,active_entry_id,redo_entry_id,undo_floor_id)) FROM sessions"); got != cursors {
		t.Fatal("rollback retained first-step cursor changes")
	}
}

func TestMessageProgressBatchesAndRebuildBoundaries(t *testing.T) {
	db := fixture(t)
	applySchema5(t, db)
	for i := 0; i < 2*messageTypeBatchSize+3; i++ {
		insert(t, db, int64(100+i), "old", 2, 0, "", "message", "assistant", `{"role":"assistant","content":"batch"}`, 150)
	}
	// Non-message status rows must not inflate conversion progress totals.
	exec(t, db, `INSERT INTO entries(session_id,actor_id,kind,model_visible,content_json,created_ms) VALUES('old','main','status',0,'{"type":"other"}',150)`)
	var count int64
	if err := db.QueryRow("SELECT count(*) FROM entries WHERE " + messageTypePredicate).Scan(&count); err != nil {
		t.Fatal(err)
	}
	tx := transaction(t, db)
	var events []Progress
	if err := Apply(tx, func(event Progress) { events = append(events, event) }); err != nil {
		t.Fatal(err)
	}
	checkProgress(t, events, 6)
	if events[0].Phase != "convert_messages" || events[0].Total != count+2 {
		t.Fatalf("incorrect conversion units: %+v, count=%d", events[0], count)
	}
	var batches, rebuild []Progress
	for _, event := range events[1:] {
		switch event.Phase {
		case "convert_messages":
			batches = append(batches, event)
		case "rebuild_entries":
			rebuild = append(rebuild, event)
		}
	}
	if len(batches) != int(count/messageTypeBatchSize) {
		t.Fatalf("callbacks not bounded by conversion batches: %+v", events)
	}
	for i, event := range batches {
		if event.Completed != int64((i+1)*messageTypeBatchSize) {
			t.Fatalf("incorrect batch progress: %+v", batches)
		}
	}
	if len(rebuild) != 2 || rebuild[0].Completed != count || rebuild[1].Completed != count+1 {
		t.Fatalf("rebuild lacks before/after progress: %+v", rebuild)
	}
}

func TestRetentionProgressIsBatchedAcrossPhases(t *testing.T) {
	db := fixture(t)
	for i := 0; i < 2*progressBatchSize+3; i++ {
		insert(t, db, int64(100+i), "old", 2, 0, "", "message", "assistant", `{"role":"assistant","content":"batch"}`, 150)
	}
	var count int64
	if err := db.QueryRow("SELECT count(*) FROM entries").Scan(&count); err != nil {
		t.Fatal(err)
	}
	tx := transaction(t, db)
	var events []Progress
	if err := Apply(tx, func(event Progress) { events = append(events, event) }); err != nil {
		t.Fatal(err)
	}
	checkProgress(t, events, 5)
	var retention []Progress
	var phases []string
	for _, event := range events {
		if event.FromVersion != 5 {
			continue
		}
		retention = append(retention, event)
		if len(phases) == 0 || phases[len(phases)-1] != event.Phase {
			phases = append(phases, event.Phase)
		}
	}
	if retention[0].Total != 4*count+2 {
		t.Fatalf("unexpected retention units: %+v, count=%d", retention[0], count)
	}
	if got := strings.Join(phases, ","); got != "read_entries,validate_retention,rewrite_retention,update_cursors,complete" {
		t.Fatal("missing retention phase", got)
	}
	if len(retention) > int(4*count/progressBatchSize)+len(phases) {
		t.Fatalf("retention reports more often than batches/phase boundaries: %+v", retention)
	}
	if retention[1].Phase != "read_entries" || retention[1].Completed != progressBatchSize {
		t.Fatalf("missing entry-reading batch progress: %+v", retention)
	}
}

func TestEmptyHistoryProgress(t *testing.T) {
	for _, from := range []int{5, 6} {
		t.Run(fmt.Sprint(from), func(t *testing.T) {
			db, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			exec(t, db, schema5)
			exec(t, db, fmt.Sprintf("PRAGMA user_version=%d", from))
			tx := transaction(t, db)
			var events []Progress
			if err := Apply(tx, func(event Progress) { events = append(events, event) }); err != nil {
				t.Fatal(err)
			}
			checkProgress(t, events, from)
		})
	}
}
