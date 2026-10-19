package migrate

import "database/sql"

// upgrade9 builds startup-query indexes. Each SQL operation can scan history;
// progress reports completed operations, never an estimate of SQL runtime.
func upgrade9(tx *sql.Tx, progress *progressReporter) error {
	steps := []struct {
		phase string
		sql   string
	}{
		{"index_entry_created", "CREATE INDEX entry_created ON entries(created_ms)"},
		{"index_prompt_recall", `CREATE INDEX entry_prompt_recall ON entries(id DESC)
			WHERE kind='message' AND role='user' AND actor_id='main'
			AND model_visible=1 AND source_id IS NULL
			AND json_extract(content_json,'$.type')='user_input'`},
		{"index_workspace_sessions", "CREATE INDEX session_list_workspace ON sessions(workspace_id, last_activity_ms DESC, id)"},
		{"remove_global_session_index", "DROP INDEX session_list"},
	}
	if progress != nil {
		progress.start(int64(len(steps)+1), steps[0].phase)
	}
	for i, step := range steps {
		if i != 0 {
			progress.phase(step.phase)
		}
		if _, err := tx.Exec(step.sql); err != nil {
			return err
		}
		progress.advance(1)
	}
	progress.phase("update_schema_version")
	return nil
}
