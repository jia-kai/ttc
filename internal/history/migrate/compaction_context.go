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

// upgrade12 normalizes only the historical role index for canonical compaction
// contexts. Their content (including saved continuation hints), source identity
// and immutable request/audit records remain unchanged. Runtime readers can then
// enforce the current semantic-role invariant without compatibility branches.
// Effective types match canonical JSON decoding: case-folded keys and the last
// matching non-null string, rather than SQLite's first exact-key lookup.
func upgrade12(tx *sql.Tx, progress *progressReporter) error {
	if progress != nil {
		progress.start(2, "normalize_compaction_roles")
	}
	if _, err := tx.Exec(`UPDATE entries SET role='user'
		WHERE kind IN ('message','summary') AND role='developer'
		AND (SELECT field.value FROM json_each(entries.content_json) AS field
			WHERE lower(field.key)='type' AND field.type='text'
			ORDER BY field.id DESC LIMIT 1)='compaction_context'`); err != nil {
		return err
	}
	progress.advance(1)
	progress.phase("update_schema_version")
	return nil
}
