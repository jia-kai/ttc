package history

import (
	_ "embed"
	"fmt"
	"strings"
	"testing"

	"context"
	"path/filepath"
	"ttc/internal/filelock"
	"ttc/internal/history/migrate"
	"ttc/internal/privatefile"
)

//go:embed migrate/testdata/schema5.sql
var schema5Fixture string

func TestUnsupportedVersionRejectedBeforeLifetimeLock(t *testing.T) {
	for _, version := range []int{4, schemaVersion + 1, 99} {
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
	if err := first.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("migrated version %d: %v", version, err)
	}
	second, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
}

func TestOpenDoesNotMigrateWithActiveLifetimeReader(t *testing.T) {
	for _, version := range []int{5, 10} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			testOpenWaitsForLifetimeReader(t, version)
		})
	}
}

func testOpenWaitsForLifetimeReader(t *testing.T, oldVersion int) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "data")
	if err := privatefile.PrivateDir(root); err != nil {
		t.Fatal(err)
	}
	db, err := openDatabase(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fixture := schema5Fixture
	if oldVersion == 10 {
		fixture = schema + "\nPRAGMA user_version=10;"
	}
	if _, err = db.Exec(fixture); err != nil {
		t.Fatal(err)
	}
	reader, err := filelock.AcquireShared(context.Background(), filepath.Join(root, "history-use.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	store, err := OpenWithProgress(root, func(progress migrate.Progress) {
		t.Errorf("blocked migration reported progress: %+v", progress)
	})
	if store != nil {
		store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "stop all TTC instances") {
		t.Fatalf("upgrade with live reader: %v", err)
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != oldVersion {
		t.Fatalf("blocked migration changed version %d: %v", version, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	var transitions [][2]int
	store, err = OpenWithProgress(root, func(progress migrate.Progress) {
		if progress.Phase == "complete" {
			transitions = append(transitions, [2]int{progress.FromVersion, progress.ToVersion})
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("unblocked migration version %d: %v", version, err)
	}
	if oldVersion == 10 && (len(transitions) != 1 || transitions[0] != [2]int{10, 11}) {
		t.Fatalf("version-only upgrade ran unexpected steps: %v", transitions)
	}
	second, err := OpenWithProgress(root, func(progress migrate.Progress) {
		t.Errorf("current startup reported progress: %+v", progress)
	})
	if err != nil {
		t.Fatalf("same-version store cannot share upgraded history: %v", err)
	}
	second.Close()
}
