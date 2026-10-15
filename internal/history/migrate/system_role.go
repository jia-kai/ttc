package migrate

import (
	"database/sql"
	"fmt"
	"strings"
)

// permitSystemRole rebuilds only the entries role constraint. Foreign keys keep
// referring to the stable entries name; all IDs, indexes and triggers survive.
func permitSystemRole(tx *sql.Tx) error {
	var highWater int64
	if err := tx.QueryRow("SELECT coalesce(max(seq),0) FROM sqlite_sequence WHERE name='entries'").Scan(&highWater); err != nil {
		return err
	}
	var ddl string
	if err := tx.QueryRow("SELECT sql FROM sqlite_schema WHERE type='table' AND name='entries'").Scan(&ddl); err != nil {
		return err
	}
	old := "role IN ('user','assistant','tool','developer')"
	if !strings.Contains(ddl, old) {
		return fmt.Errorf("unsupported entries role constraint")
	}
	ddl = strings.Replace(ddl, old, "role IN ('user','assistant','tool','developer','system')", 1)
	ddl = strings.Replace(ddl, "CREATE TABLE entries", "CREATE TABLE entries_message_types", 1)
	if !strings.HasPrefix(ddl, "CREATE TABLE entries_message_types") {
		return fmt.Errorf("unsupported entries table declaration")
	}
	rows, err := tx.Query("SELECT sql FROM sqlite_schema WHERE tbl_name='entries' AND type IN ('index','trigger') AND sql IS NOT NULL ORDER BY type,name")
	if err != nil {
		return err
	}
	var dependencies []string
	for rows.Next() {
		var statement string
		if err := rows.Scan(&statement); err != nil {
			rows.Close()
			return err
		}
		dependencies = append(dependencies, statement)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	// Deferral permits a transient missing referenced table inside this atomic
	// rebuild. At commit every original reference resolves to the restored name.
	if _, err := tx.Exec("PRAGMA defer_foreign_keys=ON"); err != nil {
		return err
	}
	for _, statement := range []string{ddl, "INSERT INTO entries_message_types SELECT * FROM entries"} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("extend entries roles: %w", err)
		}
	}
	if err := indexEntryReferences(tx); err != nil {
		return err
	}
	for _, statement := range []string{"DROP TABLE entries", "ALTER TABLE entries_message_types RENAME TO entries"} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("extend entries roles: %w", err)
		}
	}
	for _, index := range entryReferenceIndexes {
		// The original table's index already disappeared with DROP TABLE.
		if _, err := tx.Exec("DROP INDEX IF EXISTS " + index.name); err != nil {
			return err
		}
	}
	for _, statement := range dependencies {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("UPDATE sqlite_sequence SET seq=max(seq,?) WHERE name='entries'", highWater); err != nil {
		return err
	}
	var violations int
	if err := tx.QueryRow("SELECT count(*) FROM pragma_foreign_key_check").Scan(&violations); err != nil {
		return err
	}
	if violations != 0 {
		return fmt.Errorf("entries role migration left %d foreign key violations", violations)
	}
	// DROP TABLE's deferred violation counter is not canceled by recreating the
	// referenced name. Check the final graph explicitly before clearing deferral.
	_, err = tx.Exec("PRAGMA defer_foreign_keys=OFF")
	return err
}

var entryReferenceIndexes = []struct{ name, table, column string }{
	{"migration_original_source", "entries", "source_id"},
	{"migration_entry_parent", "entries_message_types", "parent_id"},
	{"migration_entry_source", "entries_message_types", "source_id"},
	{"migration_session_active", "sessions", "active_entry_id"},
	{"migration_session_redo", "sessions", "redo_entry_id"},
	{"migration_session_floor", "sessions", "undo_floor_id"},
	{"migration_turn_start", "turns", "start_entry_id"},
	{"migration_compaction_source", "compactions", "source_tip_id"},
	{"migration_compaction_summary", "compactions", "summary_entry_id"},
}

func indexEntryReferences(tx *sql.Tx) error {
	// DROP TABLE implicitly deletes every row with FK checks, even when
	// deferred. Every child lookup needs an index to avoid repeated table scans.
	// In particular, the replacement still references the original table, and
	// entry_source(session_id,source_id) cannot seek on source_id alone.
	// entry_parent and tool_records' integer primary key already cover the
	// remaining references. These sparse indexes exist only during the rebuild.
	for _, index := range entryReferenceIndexes {
		if _, err := tx.Exec("CREATE INDEX " + index.name + " ON " + index.table + "(" + index.column + ") WHERE " + index.column + " IS NOT NULL"); err != nil {
			return fmt.Errorf("index history references for rebuild: %w", err)
		}
	}
	return nil
}
