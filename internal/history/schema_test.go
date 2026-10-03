package history

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIncompatibleSchemaIsRejectedWithoutDeletingData(t *testing.T) {
	for _, version := range []int{0, 1, 3, 99} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "data")
			s, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
				t.Fatal(err)
			}
			s.Close()
			marker := filepath.Join(root, "openai-auth.json")
			if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			if s, err := Open(root); err == nil || !strings.Contains(err.Error(), "incompatible history schema") {
				if s != nil {
					s.Close()
				}
				t.Fatal(err)
			}
			if b, err := os.ReadFile(marker); err != nil || string(b) != "keep" {
				t.Fatal(err)
			}
		})
	}
}

func TestCurrentSchemaAndCorruptionPreserveContents(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "data")
			s, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "keep")
			if err = os.WriteFile(marker, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			if corrupt {
				if err = os.WriteFile(filepath.Join(root, "history.sqlite"), []byte("not SQLite"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			s, err = Open(root)
			if corrupt {
				if err == nil {
					s.Close()
					t.Fatal("accepted corrupt database")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
			}
			if b, err := os.ReadFile(marker); err != nil || string(b) != "keep" {
				t.Fatal("deleted compatible/error data", err)
			}
		})
	}
}

func TestHistoryDatabaseSymlinkDoesNotModifyOutsideDatabase(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside")
	s, err := Open(outside)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(outside, "history.sqlite")
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "data")
	if err = PrivateDir(root); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(source, filepath.Join(root, "history.sqlite")); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(root); err == nil {
		s.Close()
		t.Fatal("opened external history symlink")
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("modified outside database", err)
	}
	if _, err = os.Stat(filepath.Join(outside, "history.sqlite-wal")); !os.IsNotExist(err) {
		t.Fatal("opened outside WAL", err)
	}
}

func TestSQLiteSidecarSymlinksDoNotModifyOutsideFiles(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "data")
			s, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "keep")
			if err = os.WriteFile(target, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			if err = os.Symlink(target, filepath.Join(root, "history.sqlite"+suffix)); err != nil {
				t.Fatal(err)
			}
			if s, err = Open(root); err == nil {
				s.Close()
				t.Fatal("opened external sidecar symlink")
			}
			if b, err := os.ReadFile(target); err != nil || string(b) != "keep" {
				t.Fatal("modified outside sidecar", err)
			}
		})
	}
}
