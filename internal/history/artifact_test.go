package history

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func corruptArtifact(t *testing.T, path, kind string, limit int) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	var err error
	switch kind {
	case "symlink":
		target := filepath.Join(t.TempDir(), "outside")
		err = os.WriteFile(target, []byte("external sentinel"), 0600)
		if err == nil {
			err = os.Symlink(target, path)
		}
	case "fifo":
		err = syscall.Mkfifo(path, 0600)
	case "oversize":
		err = os.WriteFile(path, []byte(strings.Repeat("x", limit+1)), 0600)
	case "conflict":
		err = os.WriteFile(path, []byte("wrong"), 0600)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func assertArtifactFailure(t *testing.T, path string, call func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("unsafe or corrupt artifact accepted")
		}
	case <-time.After(time.Second):
		// Release a regressed blocking FIFO open/read so test teardown can join.
		if f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatal("artifact read blocked")
	}
}

func TestArtifactReuseRejectsUnsafeOrCorruptFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "oversize", "conflict"} {
		t.Run(kind, func(t *testing.T) {
			s, session, _, _ := historyFixture(t)
			data := []byte("original sentinel")
			path, err := s.Artifact(session.ID, "details", data)
			if err != nil {
				t.Fatal(err)
			}
			corruptArtifact(t, path, kind, len(data))
			assertArtifactFailure(t, path, func() error { _, err := s.Artifact(session.ID, "details", data); return err })
		})
	}
	s, session, _, _ := historyFixture(t)
	path, err := s.Artifact(session.ID, "details", []byte("unchanged"))
	if err != nil {
		t.Fatal(err)
	}
	if reused, err := s.Artifact(session.ID, "details", []byte("unchanged")); err != nil || reused != path {
		t.Fatal("valid immutable reuse failed", reused, err)
	}
}

func TestInstructionReadsRejectUnsafeAndOversizedSnapshots(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			s, session, turn, request := historyFixture(t)
			id, err := s.RecordSystemPrompt(session.ID, turn, "main", request, "original instructions")
			if err != nil {
				t.Fatal(err)
			}
			entry, err := s.Entry(id)
			if err != nil {
				t.Fatal(err)
			}
			var status struct{ Path string }
			if err := json.Unmarshal(entry.Content, &status); err != nil {
				t.Fatal(err)
			}
			corruptArtifact(t, status.Path, kind, instructionSnapshotBytes)
			for name, call := range map[string]func() error{
				"inspect": func() error { _, err := s.Inspect(entry); return err },
				"page":    func() error { _, err := s.InspectPage(context.Background(), id, 0, 100); return err },
				"jsonl":   func() error { _, err := s.TranscriptJSONL(session.ID, 0); return err },
				"export":  func() error { return s.Export(session.ID, filepath.Join(t.TempDir(), "transcript.md")) },
			} {
				t.Run(name, func(t *testing.T) { assertArtifactFailure(t, status.Path, call) })
			}
		})
	}
	s, session, turn, request := historyFixture(t)
	if _, err := s.RecordSystemPrompt(session.ID, turn, "main", request, strings.Repeat("x", instructionSnapshotBytes+1)); err == nil {
		t.Fatal("saved instructions larger than inspection/export can read")
	}
}
