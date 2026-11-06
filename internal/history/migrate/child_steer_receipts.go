package migrate

import "database/sql"

// upgrade14 renames canonical steering receipts, including copied and recovered
// events. Admitted messages, provider snapshots, tool results and archives remain
// exact historical evidence, even when they contain an old receipt name.
func upgrade14(tx *sql.Tx, progress *progressReporter) error {
	if progress != nil {
		progress.start(2, "rename_child_steer_receipts")
	}
	_, err := tx.Exec(`UPDATE entries SET content_json=json_set(content_json,'$.body.type',
		CASE json_extract(content_json,'$.body.type')
			WHEN 'child_message_delivered' THEN 'child_steer_delivered'
			WHEN 'child_message_undelivered' THEN 'child_steer_undelivered'
		END)
		WHERE kind='status' AND json_extract(content_json,'$.type')='runtime_event'
		AND json_extract(content_json,'$.body.type') IN ('child_message_delivered','child_message_undelivered')`)
	if err != nil {
		return err
	}
	progress.advance(1)
	progress.phase("update_schema_version")
	return nil
}
