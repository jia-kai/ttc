package history

import (
	"os"
	"syscall"
	"testing"
	"time"

	"ttc/internal/llm"
)

func TestContinuationAndReloadNeedNoArchives(t *testing.T) {
	for _, legacyArchive := range []bool{false, true} {
		t.Run(map[bool]string{false: "never_created", true: "deleted"}[legacyArchive], func(t *testing.T) {
			s, v, turn, _ := historyFixture(t)
			retained, err := s.Append(v.ID, turn, "main", "message", "user", true, llm.Message{Type: llm.TypeUserInput, Content: "Retained task"})
			if err != nil {
				t.Fatal(err)
			}
			if legacyArchive {
				path, err := s.ArchiveActorTranscript(v.ID, "main", nil)
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range []string{path, path + ".jsonl"} {
					if err := os.Remove(p); err != nil {
						t.Fatal(err)
					}
				}
			}
			next, err := s.Continue(v.ID, "Summary", retained, retained, nil, time.Now(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Load(next.ID); err != nil {
				t.Fatal("reload required generated archives", err)
			}
		})
	}
}

func TestArchiveWriterRejectsExistingSpecialFile(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	archive, err := s.ArchiveActorTranscript(v.ID, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(archive, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ArchiveActorTranscript(v.ID, "main", nil); err == nil {
		t.Fatal("special archive file accepted")
	}
}
