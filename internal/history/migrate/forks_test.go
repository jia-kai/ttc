package migrate

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestSchema13ForkMetadataIsAdditiveWithoutHistoryWrites(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec(t, db, `CREATE TABLE sessions(id TEXT PRIMARY KEY,legacy TEXT);
 CREATE TABLE entries(id INTEGER PRIMARY KEY,kind TEXT,content_json TEXT);
 INSERT INTO sessions VALUES('source','unchanged');
 PRAGMA user_version=13;`)
	payload := fmt.Sprintf(`{"type":"assistant","content":%q}`, strings.Repeat("large canonical evidence", 1<<15))
	exec(t, db, "INSERT INTO entries VALUES(1,'message',?)", payload)
	before := scalar(t, db, "SELECT total_changes()")
	tx := transaction(t, db)
	var progress []Progress
	if err := Apply(tx, func(event Progress) { progress = append(progress, event) }); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	checkProgress(t, progress, 13)
	if got := scalar(t, db, "SELECT total_changes()"); got != before {
		t.Fatal("metadata migration wrote historical rows", got, before)
	}
	if got := scalar(t, db, "SELECT content_json FROM entries WHERE id=1"); got != payload {
		t.Fatal("metadata migration changed payload")
	}
	if got := scalar(t, db, "SELECT legacy FROM sessions WHERE id='source'"); got != "unchanged" {
		t.Fatal("metadata migration changed session evidence", got)
	}
	if got := scalar(t, db, "SELECT base_session_id IS NULL AND base_entry_id IS NULL FROM sessions"); got != "1" {
		t.Fatal("migration inferred legacy fork links", got)
	}
	if got := scalar(t, db, "PRAGMA user_version"); got != fmt.Sprint(CurrentVersion) {
		t.Fatal(got)
	}
	if got := scalar(t, db, "SELECT group_concat(name,',') FROM pragma_table_info('sessions')"); got != "id,legacy,base_session_id,base_entry_id" {
		t.Fatal("unexpected additive column order", got)
	}
	var metadataProgress []Progress
	for _, event := range progress {
		if event.FromVersion == 13 {
			metadataProgress = append(metadataProgress, event)
		}
	}
	if len(metadataProgress) != 3 || metadataProgress[0].Total != 3 || metadataProgress[0].Phase != "add_fork_metadata" {
		t.Fatal("metadata migration work depends on history size", metadataProgress)
	}
}
