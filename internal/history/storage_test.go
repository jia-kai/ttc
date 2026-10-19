package history

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func physicalHistoryBytes(t *testing.T, s *Store) int64 {
	t.Helper()
	var total int64
	for _, suffix := range []string{"", "-wal"} {
		info, err := os.Stat(filepath.Join(s.Root, "history.sqlite") + suffix)
		if os.IsNotExist(err) && suffix == "-wal" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	return total
}

func TestStorageStatsIncludesWALAndAllEntryKinds(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	if _, err := s.db.Exec(`INSERT INTO entries(session_id,actor_id,kind,model_visible,content_json,created_ms) VALUES(?,'main','status',0,'{}',-123)`, v.ID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"export.md", "openai-auth.json", "history.sqlite-shm-extra"} {
		if err := os.WriteFile(filepath.Join(s.Root, name), make([]byte, 10000), 0600); err != nil {
			t.Fatal(err)
		}
	}
	wal, err := os.Stat(filepath.Join(s.Root, "history.sqlite-wal"))
	if err != nil || wal.Size() == 0 {
		t.Fatal("fixture must have a nonempty WAL", wal, err)
	}
	stats, err := s.StorageStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := s.db.QueryRow("SELECT count(*) FROM entries").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if stats.Bytes != physicalHistoryBytes(t, s) || stats.Entries != count || stats.OldestMS != -123 {
		t.Fatalf("incorrect stats: %+v count=%d", stats, count)
	}
}

func TestStorageStatsEmptyVersusEpoch(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stats, err := s.StorageStats(context.Background())
	if err != nil || stats.Entries != 0 || stats.OldestMS != 0 || stats.Bytes <= 0 {
		t.Fatal(stats, err)
	}
	model := testSelection()
	v, _ := startHistorySession(t, s, t.TempDir(), model, "epoch")
	if _, err := s.db.Exec("UPDATE entries SET created_ms=0 WHERE session_id=?", v.ID); err != nil {
		t.Fatal(err)
	}
	stats, err = s.StorageStats(context.Background())
	if err != nil || stats.Entries == 0 || stats.OldestMS != 0 {
		t.Fatal(stats, err)
	}
}

func TestStorageStatsSurvivingIDSpanAndOldestTimestamp(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries [][2]int64 // ID and creation time in Unix milliseconds.
		remove  []int64
		span    int64
		oldest  int64
	}{
		{name: "single high ID", entries: [][2]int64{{50, 123}}, span: 1, oldest: 123},
		{name: "backdated middle ID", entries: [][2]int64{{50, 100}, {51, -123}, {52, 200}}, span: 3, oldest: -123},
		{name: "interior gap", entries: [][2]int64{{50, 100}, {51, -123}, {52, 200}}, remove: []int64{51}, span: 3, oldest: 100},
		{name: "deleted endpoints", entries: [][2]int64{{50, -300}, {51, 100}, {52, 200}, {53, -200}}, remove: []int64{50, 53}, span: 2, oldest: 100},
		{name: "all deleted", entries: [][2]int64{{50, 0}}, remove: []int64{50}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "data"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err := s.db.Exec(`INSERT INTO workspaces(id,path) VALUES('w','/fixture');
				INSERT INTO sessions(id,workspace_id,lineage_id,name,name_source,model_json,last_activity_ms,metadata_json)
				VALUES('s','w','s','fixture','default','{}',0,'{}')`); err != nil {
				t.Fatal(err)
			}
			for _, entry := range tc.entries {
				if _, err := s.db.Exec(`INSERT INTO entries(id,session_id,actor_id,kind,model_visible,content_json,created_ms)
					VALUES(?,'s','main','status',0,'{}',?)`, entry[0], entry[1]); err != nil {
					t.Fatal(err)
				}
			}
			for _, id := range tc.remove {
				if _, err := s.db.Exec("DELETE FROM entries WHERE id=?", id); err != nil {
					t.Fatal(err)
				}
			}
			stats, err := s.StorageStats(context.Background())
			if err != nil || stats.Entries != tc.span || stats.OldestMS != tc.oldest {
				t.Fatalf("stats=%+v, err=%v; want span=%d oldest=%d", stats, err, tc.span, tc.oldest)
			}
		})
	}
}

func TestStorageBytesMissingWALAndInvalidFiles(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	main := filepath.Join(s.Root, "history.sqlite")
	if err := os.WriteFile(main, []byte("file length"), 0600); err != nil {
		t.Fatal(err)
	}
	if size, err := s.storageBytes(context.Background()); err != nil || size != 11 {
		t.Fatal(size, err)
	}
	if err := os.Symlink(main, main+"-wal"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.storageBytes(context.Background()); err == nil {
		t.Fatal("accepted a WAL symlink")
	}
	if err := os.Remove(main); err != nil {
		t.Fatal(err)
	}
	if _, err := s.storageBytes(context.Background()); err == nil {
		t.Fatal("missing main database was treated as zero bytes")
	}
}

func TestStorageStatsCancellation(t *testing.T) {
	s, _, _, _ := historyFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.StorageStats(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
