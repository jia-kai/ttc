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
	for _, statement := range []string{ddl, "INSERT INTO entries_message_types SELECT * FROM entries", "DROP TABLE entries", "ALTER TABLE entries_message_types RENAME TO entries"} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("extend entries roles: %w", err)
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
