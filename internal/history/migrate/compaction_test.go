package migrate

import (
	"strings"
	"testing"
)

func TestArchiveMetadataMigrationPreservesCutsAndEvidence(t *testing.T) {
	db := fixture(t)
	// Upgrade all semantic evidence first, then restore the schema-8 columns.
	applyCurrent(t, db)
	resetSchema7(t, db)
	exec(t, db, "PRAGMA user_version=8")
	before := scalar(t, db, "SELECT json_group_array(json_array(id,source_id,content_json)) FROM entries")
	cuts := scalar(t, db, "SELECT json_group_array(json_array(continuation_id,predecessor_id,source_tip_id,ordinal,summary_entry_id)) FROM compactions")
	requests := scalar(t, db, "SELECT json_group_array(json_array(id,input_json)) FROM model_requests")
	applyCurrent(t, db)
	if got := scalar(t, db, "SELECT json_group_array(json_array(id,source_id,content_json)) FROM entries"); got != before {
		t.Fatal("immutable entries changed")
	}
	if got := scalar(t, db, "SELECT json_group_array(json_array(continuation_id,predecessor_id,source_tip_id,ordinal,summary_entry_id)) FROM compactions"); got != cuts {
		t.Fatal("frozen cuts changed")
	}
	if got := scalar(t, db, "SELECT json_group_array(json_array(id,input_json)) FROM model_requests"); got != requests {
		t.Fatal("saved requests changed")
	}
	columns := scalar(t, db, "SELECT group_concat(name) FROM pragma_table_info('compactions')")
	if strings.Contains(columns, "archive") {
		t.Fatal("obsolete archive metadata retained", columns)
	}
}
