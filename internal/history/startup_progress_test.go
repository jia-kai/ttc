package history

import (
	"path/filepath"
	"reflect"
	"testing"

	"ttc/internal/history/migrate"
	"ttc/internal/privatefile"
)

func TestOpenWithProgressReportsOrderedUncommittedUpgrades(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	if err := privatefile.PrivateDir(root); err != nil {
		t.Fatal(err)
	}
	db, err := openDatabase(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema5Fixture); err != nil {
		t.Fatal(err)
	}
	var transitions [][2]int
	reports := 0
	store, err := OpenWithProgress(root, func(progress migrate.Progress) {
		reports++
		transition := [2]int{progress.FromVersion, progress.ToVersion}
		if len(transitions) == 0 || transitions[len(transitions)-1] != transition {
			transitions = append(transitions, transition)
		}
		// Reports describe transaction work, not visibility to other readers.
		var version int
		if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 5 {
			t.Errorf("reported migration committed before open returned: version=%d err=%v", version, err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if reports == 0 || !reflect.DeepEqual(transitions, [][2]int{{5, 6}, {6, 7}, {7, 8}}) {
		t.Fatalf("unexpected migration reports: count=%d transitions=%v", reports, transitions)
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("open did not commit the upgrade: version=%d err=%v", version, err)
	}
}

func TestOpenWithProgressSilentForEmptyAndCurrentStores(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	for i := 0; i < 2; i++ {
		reports := 0
		store, err := OpenWithProgress(root, func(migrate.Progress) { reports++ })
		if err != nil {
			t.Fatal(err)
		}
		store.Close()
		if reports != 0 {
			t.Fatalf("open %d emitted %d reports without an upgrade", i, reports)
		}
	}
}

func TestOpenWithProgressErrorRollsBackUpgrade(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	if err := privatefile.PrivateDir(root); err != nil {
		t.Fatal(err)
	}
	db, err := openDatabase(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema5Fixture + "\nDROP TABLE entries;"); err != nil {
		t.Fatal(err)
	}
	store, err := OpenWithProgress(root, func(migrate.Progress) {})
	if store != nil {
		store.Close()
	}
	if err == nil {
		t.Fatal("opened malformed migration fixture")
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 5 {
		t.Fatalf("failed open committed an upgrade: version=%d err=%v", version, err)
	}
}
