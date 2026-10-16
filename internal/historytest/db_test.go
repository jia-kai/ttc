package historytest_test

import (
	"context"
	"path/filepath"
	"testing"

	"ttc/internal/history"
	"ttc/internal/historytest"
	"ttc/internal/llm"
)

func TestIndependentSQLAssertionsAndFaultInjection(t *testing.T) {
	store, err := history.Open(filepath.Join(t.TempDir(), "data with space"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	db := historytest.DB(t, store)
	if db != historytest.DB(t, store) {
		t.Fatal("helper did not reuse the test connection")
	}
	var foreignKeys int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatal(foreignKeys, err)
	}
	id := history.NewID("session")
	if _, _, err := store.StartSession(id, t.TempDir(), llm.Selection{Provider: "script", Model: llm.ScriptModel()}, llm.Message{Type: llm.TypeUserInput, Content: "Test input"}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM sessions WHERE id=?", id).Scan(&count); err != nil || count != 1 {
		t.Fatal("separate connection cannot see committed storage", count, err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_name BEFORE UPDATE OF name ON sessions BEGIN SELECT RAISE(ABORT,'test name failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.RenameSession(id, "Manual name"); err == nil {
		t.Fatal("test fault injection did not affect storage operation")
	}
	if _, err := db.Exec("DROP TRIGGER fail_name"); err != nil {
		t.Fatal(err)
	}
	lock, err := historytest.LockWriter(t, store, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A WAL reader can still inspect committed records while writes are blocked.
	if _, err := store.Session(id); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal("lock close is not idempotent", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.RenameSession(id, "Still usable"); err != nil {
		t.Fatal("test helper exposed the Store connection pool", err)
	}
}
