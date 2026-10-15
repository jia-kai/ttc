package migrate

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

const entryReferenceTables = `
CREATE TABLE sessions (active_entry_id INTEGER REFERENCES entries(id), redo_entry_id INTEGER REFERENCES entries(id), undo_floor_id INTEGER REFERENCES entries(id));
CREATE TABLE turns (start_entry_id INTEGER REFERENCES entries(id));
CREATE TABLE compactions (source_tip_id INTEGER REFERENCES entries(id), summary_entry_id INTEGER REFERENCES entries(id));`

func TestSystemRoleRebuildPreservesDeletedEntryHighWater(t *testing.T) {
	for _, empty := range []bool{false, true} {
		name := "surviving rows"
		if empty {
			name = "empty table"
		}
		t.Run(name, func(t *testing.T) {
			db, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			exec(t, db, `CREATE TABLE entries (id INTEGER PRIMARY KEY AUTOINCREMENT, parent_id INTEGER REFERENCES entries(id), source_id INTEGER REFERENCES entries(id), role TEXT CHECK (role IN ('user','assistant','tool','developer')))`)
			exec(t, db, entryReferenceTables)
			exec(t, db, `INSERT INTO entries(id,role) VALUES(1,'user'),(1000,'user')`)
			exec(t, db, `DELETE FROM entries WHERE id=1000`)
			if empty {
				exec(t, db, `DELETE FROM entries`)
			}
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := permitSystemRole(tx); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			result, err := db.Exec(`INSERT INTO entries(role) VALUES('system')`)
			if err != nil {
				t.Fatal(err)
			}
			id, err := result.LastInsertId()
			if err != nil || id <= 1000 {
				t.Fatalf("migration reused an archived identity: id=%d err=%v", id, err)
			}
		})
	}
}

func TestSystemRoleRebuildPreservesSelfReferences(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	exec(t, db, `PRAGMA foreign_keys=ON`)
	exec(t, db, `CREATE TABLE entries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		parent_id INTEGER REFERENCES entries(id),
		source_id INTEGER REFERENCES entries(id),
		role TEXT CHECK (role IN ('user','assistant','tool','developer')))`)
	exec(t, db, `CREATE INDEX entry_parent ON entries(parent_id)`)
	exec(t, db, entryReferenceTables)
	// The normal source index starts with session_id in the full schema and
	// cannot serve a lookup on source_id alone. Neither replacement lookup has
	// a permanent usable index while the original table is being dropped.
	exec(t, db, `WITH RECURSIVE ids(id) AS (VALUES(1) UNION ALL SELECT id+1 FROM ids WHERE id<4096)
		INSERT INTO entries(id,parent_id,source_id,role) SELECT id,NULLIF(id-1,0),CASE WHEN id>1 THEN 1 END,'user' FROM ids`)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := permitSystemRole(tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := scalar(t, db, `SELECT count(*) FROM entries WHERE (id=1 AND parent_id IS NULL AND source_id IS NULL) OR (id>1 AND parent_id=id-1 AND source_id=1)`); got != "4096" {
		t.Fatal("self-references changed", got)
	}
	if got := scalar(t, db, `SELECT count(*) FROM pragma_foreign_key_check`); got != "0" {
		t.Fatal("invalid references", got)
	}
	if got := scalar(t, db, `SELECT count(*) FROM sqlite_schema WHERE name LIKE 'migration_%' OR name='entries_message_types'`); got != "0" {
		t.Fatal("migration objects leaked", got)
	}
	if got := scalar(t, db, `SELECT count(*) FROM sqlite_schema WHERE name='entry_parent'`); got != "1" {
		t.Fatal("permanent index lost", got)
	}
	if _, err := db.Exec(`INSERT INTO entries(parent_id,role) VALUES(99999,'system')`); err == nil {
		t.Fatal("foreign-key enforcement disabled after rebuild")
	}
}

// BenchmarkSystemRoleRebuild exposes accidental per-entry scans of the full
// replacement table without imposing machine-dependent timing limits in tests.
func BenchmarkSystemRoleRebuild(b *testing.B) {
	for _, size := range []int{2000, 20000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			db, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			if _, err := db.Exec(`PRAGMA foreign_keys=ON;
				CREATE TABLE entries (id INTEGER PRIMARY KEY AUTOINCREMENT, parent_id INTEGER REFERENCES entries(id), source_id INTEGER REFERENCES entries(id), role TEXT CHECK (role IN ('user','assistant','tool','developer')));
				CREATE INDEX entry_parent ON entries(parent_id);` + entryReferenceTables); err != nil {
				b.Fatal(err)
			}
			if _, err := db.Exec(`WITH RECURSIVE ids(id) AS (VALUES(1) UNION ALL SELECT id+1 FROM ids WHERE id<?)
				INSERT INTO entries(id,parent_id,source_id,role) SELECT id,NULLIF(id-1,0),CASE WHEN id>1 THEN 1 END,'user' FROM ids`, size); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for b.Loop() {
				tx, err := db.Begin()
				if err != nil {
					b.Fatal(err)
				}
				if err := permitSystemRole(tx); err != nil {
					tx.Rollback()
					b.Fatal(err)
				}
				// Rollback restores the legacy constraint for the next iteration.
				if err := tx.Rollback(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestEntryRebuildForeignKeyLookupsAreIndexed(t *testing.T) {
	db := fixture(t)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var ddl string
	if err := tx.QueryRow(`SELECT sql FROM sqlite_schema WHERE name='entries'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(strings.Replace(ddl, "CREATE TABLE entries", "CREATE TABLE entries_message_types", 1)); err != nil {
		t.Fatal(err)
	}
	if err := indexEntryReferences(tx); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(`EXPLAIN QUERY PLAN DELETE FROM entries WHERE id=?`, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	searches := map[string]bool{}
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(detail, "SCAN ") {
			t.Errorf("rebuild would scan a child table for every entry: %s", detail)
		}
		fields := strings.Fields(detail)
		if len(fields) >= 2 && fields[0] == "SEARCH" {
			searches[fields[1]] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"entries", "entries_message_types", "sessions", "turns", "compactions", "tool_records"} {
		if !searches[table] {
			t.Errorf("missing indexed foreign-key lookup for %s", table)
		}
	}
}
