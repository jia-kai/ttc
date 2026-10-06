package history

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"ttc/internal/provider"
)

func TestContinuationRequiresBothArchiveFilesBeforeCommit(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "fifo", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			s, v, turn, _ := historyFixture(t)
			retained, err := s.Append(v.ID, turn, "main", "message", "user", true, provider.Message{Role: "user", Content: "Retained task"})
			if err != nil {
				t.Fatal(err)
			}
			archive, err := s.ArchiveTranscript(v.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(archive + ".jsonl")
			if err != nil {
				t.Fatal(err)
			}
			if mode != "corrupt" {
				if err := os.Remove(archive + ".jsonl"); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "fifo":
				if err := syscall.Mkfifo(archive+".jsonl", 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(archive, archive+".jsonl"); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(archive+".jsonl", original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "corrupt" {
				continued, err := s.Continue(v.ID, "Summary", archive, retained, nil, time.Now(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(archive+".jsonl", []byte("corrupted exact history"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := s.ValidateArchive(continued.ID); err == nil || !strings.Contains(err.Error(), "corrupt") {
					t.Fatal("corrupt sidecar accepted", err)
				}
				if _, err := s.Load(continued.ID); err == nil {
					t.Fatal("corrupt sidecar loaded")
				}
			} else {
				if _, err := s.Continue(v.ID, "Summary", archive, retained, nil, time.Now(), nil); err == nil {
					t.Fatal("invalid required sidecar accepted")
				}
				old, err := s.Session(v.ID)
				if err != nil || old.ReadOnly {
					t.Fatal("failed archive validation froze source", old, err)
				}
			}
		})
	}
}

func TestDeletedExactArchiveFailsReload(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	retained, err := s.Append(v.ID, turn, "main", "message", "user", true, provider.Message{Role: "user", Content: "Retained task"})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := s.ArchiveTranscript(v.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := s.Continue(v.ID, "Summary", archive, retained, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(archive + ".jsonl"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(continued.ID); err == nil {
		t.Fatal("missing sidecar accepted on reload")
	}
}

func TestArchiveWriterRejectsExistingSpecialFile(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	archive, err := s.ArchiveTranscript(v.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(archive, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ArchiveTranscript(v.ID, 0); err == nil {
		t.Fatal("special archive file accepted")
	}
}
