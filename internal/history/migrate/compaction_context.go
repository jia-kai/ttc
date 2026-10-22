package migrate

import "database/sql"

// upgrade10 fences the compaction_context message vocabulary from older strict
// decoders. Apply advances user_version; existing records and layout stay intact.
func upgrade10(_ *sql.Tx, progress *progressReporter) error {
	if progress != nil {
		progress.start(1, "update_schema_version")
	}
	return nil
}
