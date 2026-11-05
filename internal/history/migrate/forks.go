package migrate

import "database/sql"

// upgrade13 adds manual-fork metadata without reading or rewriting history. Keep
// column order aligned with schema.sql; existing eager copies remain unchanged.
func upgrade13(tx *sql.Tx, progress *progressReporter) error {
	if progress != nil {
		progress.start(3, "add_fork_metadata")
	}
	if _, err := tx.Exec("ALTER TABLE sessions ADD COLUMN base_session_id TEXT REFERENCES sessions(id)"); err != nil {
		return err
	}
	progress.advance(1)
	if _, err := tx.Exec("ALTER TABLE sessions ADD COLUMN base_entry_id INTEGER REFERENCES entries(id)"); err != nil {
		return err
	}
	progress.advance(1)
	progress.phase("update_schema_version")
	return nil
}
