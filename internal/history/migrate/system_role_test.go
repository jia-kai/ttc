package migrate

import (
	"database/sql"
	"testing"
)

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
			exec(t, db, `CREATE TABLE entries (id INTEGER PRIMARY KEY AUTOINCREMENT, role TEXT CHECK (role IN ('user','assistant','tool','developer')))`)
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
