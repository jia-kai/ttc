package history

import (
	_ "embed"
	"fmt"
	"strings"
	"testing"

	"context"
	"path/filepath"
	"ttc/internal/filelock"
	"ttc/internal/privatefile"
)

//go:embed migrate/testdata/schema5.sql
var schema5Fixture string

func TestUnsupportedVersionRejectedBeforeLifetimeLock(t *testing.T) {
	for _, version := range []int{4, 99} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "data")
			if err := privatefile.PrivateDir(root); err != nil {
				t.Fatal(err)
			}
			db, err := openDatabase(root)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
				t.Fatal(err)
			}
			reader, err := filelock.AcquireShared(context.Background(), filepath.Join(root, "history-use.lock"))
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			store, err := Open(root)
			if store != nil {
				store.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "incompatible history schema") {
				t.Fatalf("unsupported version waited for upgrade: %v", err)
			}
		})
	}
}

func TestOpenMigratesSchema5AndIsIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	if err := privatefile.PrivateDir(root); err != nil {
		t.Fatal(err)
	}
	db, err := openDatabase(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(schema5Fixture); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	first, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	var version int
	if err := first.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("migrated version %d: %v", version, err)
	}
	second, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
}

func TestOpenDoesNotMigrateWithActiveLifetimeReader(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	if err := privatefile.PrivateDir(root); err != nil {
		t.Fatal(err)
	}
	db, err := openDatabase(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(schema5Fixture); err != nil {
		t.Fatal(err)
	}
	reader, err := filelock.AcquireShared(context.Background(), filepath.Join(root, "history-use.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	store, err := Open(root)
	if store != nil {
		store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "stop all TTC instances") {
		t.Fatalf("upgrade with live reader: %v", err)
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 5 {
		t.Fatalf("blocked migration changed version %d: %v", version, err)
	}
}
