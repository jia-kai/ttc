package migrate

import "database/sql"

// upgrade8 drops generated archive metadata, preserving frozen cuts and original
// summaries. Old archive files remain optional artifacts, never load prerequisites.
func upgrade8(tx *sql.Tx, progress *progressReporter) error {
	if progress != nil {
		progress.start(5, "remove_archive_metadata")
	}
	for _, column := range []string{"archive_path", "archive_sha256", "archive_exact_sha256"} {
		if _, err := tx.Exec("ALTER TABLE compactions DROP COLUMN " + column); err != nil {
			return err
		}
		progress.advance(1)
	}
	if _, err := tx.Exec("CREATE UNIQUE INDEX compaction_summary ON compactions(summary_entry_id)"); err != nil {
		return err
	}
	progress.advance(1)
	return nil
}
