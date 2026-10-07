package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ttc/internal/llm"
)

func TestArchiveActorTranscriptIncludesOriginalCompactionAncestryAndPreviousAssignments(t *testing.T) {
	s, initial, turn, _ := historyFixture(t)
	actor := "main/child_audit"
	appendMessage := func(session, role, text string) int64 {
		t.Helper()
		id, err := s.Append(session, turn, actor, "message", role, false, llm.Message{Role: role, Content: text})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := appendMessage(initial.ID, "user", "First persistent assignment")
	firstAnswer := appendMessage(initial.ID, "assistant", "Original first answer before compaction")
	// Continuation removes native replay state from this retained copy. The
	// actor export must still prefer the original entry's exact provider payload.
	retained, err := s.Append(initial.ID, turn, actor, "message", "assistant", true, llm.Message{
		Role: "assistant", Content: "Retained child tail",
		State: &llm.ReplayState{Provider: "script", Model: "scripted", Version: 1, Items: []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"original-native-state"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	original, err := s.Entry(retained)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := s.ArchiveTranscript(initial.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := s.Continue(initial.ID, "Main summary omits child details", archive, retained, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	copies, err := s.Branch(continued.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	copied := false
	for _, entry := range copies {
		if entry.Source != retained {
			continue
		}
		var message llm.Message
		if err := json.Unmarshal(entry.Content, &message); err != nil {
			t.Fatal(err)
		}
		if message.State != nil || bytes.Equal(entry.Content, original.Content) {
			t.Fatal("fixture did not change copied replay state", message)
		}
		copied = true
	}
	if !copied {
		t.Fatal("fixture did not retain the native-state entry")
	}
	second := appendMessage(continued.ID, "user", "Second persistent assignment")
	partial := appendMessage(continued.ID, "assistant", "Last partial response")
	if _, err := s.Append(continued.ID, turn, "main/other_child", "message", "assistant", false, llm.Message{Role: "assistant", Content: "Unrelated sibling evidence"}); err != nil {
		t.Fatal(err)
	}
	// Archive/continue once more without retaining child entries. A current-branch
	// exporter would now see none of the child's original conversation.
	archive, err = s.ArchiveTranscript(continued.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	continued, err = s.Session(continued.ID)
	if err != nil {
		t.Fatal(err)
	}
	continued, err = s.Continue(continued.ID, "Second main summary", archive, continued.EntryTip+1, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	path, err := s.ArchiveActorTranscript(continued.ID, actor, nil)
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"First persistent assignment", "Original first answer before compaction", "Retained child tail", "Second persistent assignment", "Last partial response"} {
		if !bytes.Contains(markdown, []byte(text)) {
			t.Fatal("original conversation missing from Markdown", text)
		}
	}
	if bytes.Contains(markdown, []byte("Unrelated sibling")) || bytes.Contains(markdown, []byte("Main summary")) {
		t.Fatal("unrelated actor or summary leaked into child export", string(markdown))
	}
	exact, err := os.ReadFile(path + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(exact))
	var ids []int64
	for scanner.Scan() {
		var line struct {
			Entry Entry `json:"entry"`
			Seq   int64 `json:"event_seq"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		if line.Entry.Actor != actor || line.Seq != line.Entry.ID || line.Entry.Source != 0 {
			t.Fatal("export did not prefer original exact entry", line)
		}
		if line.Seq == retained && !bytes.Equal(line.Entry.Content, original.Content) {
			t.Fatal("original envelope content changed", line.Entry.Content, original.Content)
		}
		ids = append(ids, line.Seq)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	want := []int64{first, firstAnswer, retained, second, partial}
	if len(ids) != len(want) {
		t.Fatal("copies duplicated or original entries lost", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatal("wrong conversation chronology", ids, want)
		}
	}
	for _, name := range []string{path, path + ".jsonl"} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("artifact is not private", name, info, err)
		}
	}
}

func TestArchiveActorTranscriptRejectsEmptyActor(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	if path, err := s.ArchiveActorTranscript(v.ID, " \t", nil); err == nil || path != "" || !strings.Contains(err.Error(), "actor") {
		t.Fatal(path, err)
	}
}

func TestArchiveActorTranscriptFollowsSelectedCutsOnly(t *testing.T) {
	s, initial, turn, _ := historyFixture(t)
	actor := "main/child_audit"
	appendMessage := func(session, owner, text string) int64 {
		t.Helper()
		id, err := s.Append(session, turn, owner, "message", "assistant", false, llm.Message{Role: "assistant", Content: text})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	selectTip := func(session string, tip int64) {
		t.Helper()
		if _, err := s.DB.Exec("UPDATE sessions SET active_entry_id=? WHERE id=?", tip, session); err != nil {
			t.Fatal(err)
		}
	}
	first := appendMessage(initial.ID, actor, "Selected first")
	abandoned := appendMessage(initial.ID, actor, "Abandoned predecessor branch")
	selectTip(initial.ID, first)
	appendMessage(initial.ID, "main", "Unrelated connector")
	second := appendMessage(initial.ID, actor, "Selected second")
	tip := appendMessage(initial.ID, "main/other_child", "Unrelated sibling actor")
	archive, err := s.ArchiveTranscript(initial.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := s.Continue(initial.ID, "Main summary", archive, tip+1, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The compaction's source cut, not the predecessor's current selected tip,
	// defines the old conversation included in the export.
	selectTip(initial.ID, abandoned)
	third := appendMessage(continued.ID, actor, "Selected current")
	appendMessage(continued.ID, actor, "Abandoned current branch")
	selectTip(continued.ID, third)
	appendMessage(continued.ID, "main", "Unrelated current tip")
	snapshot, err := s.Load(continued.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		session string
		want    []int64
	}{
		{"continuation", continued.ID, []int64{first, second, third}},
		// A manual snapshot inherits only copied selected entries, not the
		// compaction ancestry of its source conversation.
		{"manual_snapshot", snapshot.ID, []int64{third}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, err := s.ArchiveActorTranscript(tc.session, actor, nil)
			if err != nil {
				t.Fatal(err)
			}
			exact, err := os.ReadFile(path + ".jsonl")
			if err != nil {
				t.Fatal(err)
			}
			scanner := bufio.NewScanner(bytes.NewReader(exact))
			var ids []int64
			for scanner.Scan() {
				var line struct {
					Entry Entry `json:"entry"`
					Seq   int64 `json:"event_seq"`
				}
				if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
					t.Fatal(err)
				}
				if line.Entry.Actor != actor {
					t.Fatal("unrelated actor exported", line.Entry)
				}
				ids = append(ids, line.Seq)
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if len(ids) != len(tc.want) {
				t.Fatal("wrong selected conversation", ids, tc.want)
			}
			for i := range ids {
				if ids[i] != tc.want[i] {
					t.Fatal("wrong selected chronology", ids, tc.want)
				}
			}
		})
	}
}

func BenchmarkArchiveActorTranscriptUnrelatedPayload(b *testing.B) {
	for _, compacted := range []bool{false, true} {
		for _, size := range []int{0, 8 << 20} {
			name := "current/"
			if compacted {
				name = "compacted/"
			}
			if size == 0 {
				name += "small_main"
			} else {
				name += "8MiB_main"
			}
			b.Run(name, func(b *testing.B) {
				s, err := Open(filepath.Join(b.TempDir(), "data"))
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { s.Close() })
				session := NewID("session")
				model := llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}
				turn, _, err := s.StartSession(session, b.TempDir(), model, llm.Message{Role: "user", Content: strings.Repeat("x", size)})
				if err != nil {
					b.Fatal(err)
				}
				actor := "main/child_audit"
				child, err := s.Append(session, turn, actor, "message", "assistant", false, llm.Message{Role: "assistant", Content: "Tiny child answer"})
				if err != nil {
					b.Fatal(err)
				}
				if compacted {
					archive, err := s.ArchiveTranscript(session, 0)
					if err != nil {
						b.Fatal(err)
					}
					continued, err := s.Continue(session, "Main summary", archive, child, nil, time.Now(), nil)
					if err != nil {
						b.Fatal(err)
					}
					session = continued.ID
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if _, err := s.ArchiveActorTranscript(session, actor, nil); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
