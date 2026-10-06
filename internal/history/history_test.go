package history

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"ttc/internal/provider"
	"ttc/internal/render"
)

func TestGeneratedIDsAreCompactOpaqueAndURLSafe(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := NewID("job")
		if len(id) != len("job_")+16 || !strings.HasPrefix(id, "job_") || seen[id] {
			t.Fatal(id)
		}
		decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, "job_"))
		if err != nil || len(decoded) != 12 {
			t.Fatal(id, err)
		}
		for _, r := range id {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				t.Fatal("non-URL-safe ID", id)
			}
		}
		seen[id] = true
	}
}

func TestSessionsFiltersWorkspaceBeforeLimit(t *testing.T) {
	s, local, _, _ := historyFixture(t)
	if _, err := s.DB.Exec("UPDATE sessions SET last_activity_ms=1 WHERE id=?", local.ID); err != nil {
		t.Fatal(err)
	}
	foreignRoot := t.TempDir()
	for range 101 {
		startHistorySession(t, s, foreignRoot, local.Model, "foreign message")
	}
	var localRoot string
	if err := s.DB.QueryRow("SELECT path FROM workspaces WHERE id=?", local.WorkspaceID).Scan(&localRoot); err != nil {
		t.Fatal(err)
	}
	list, err := s.Sessions(localRoot)
	if err != nil || len(list) != 1 || list[0].ID != local.ID {
		t.Fatal("other workspaces hid local history", list, err)
	}
	if _, err := s.Sessions(""); err == nil {
		t.Fatal("accepted missing workspace")
	}
}

func TestExportPreservesImageMetadataBytes(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	lineage := filepath.Join(s.Root, "lineages", v.LineageID) + string(filepath.Separator)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	// A valid text chunk deliberately contains the path that text artifacts rewrite.
	data := []byte("Comment\x00" + lineage)
	chunk := make([]byte, 12+len(data))
	binary.BigEndian.PutUint32(chunk, uint32(len(data)))
	copy(chunk[4:8], "tEXt")
	copy(chunk[8:], data)
	binary.BigEndian.PutUint32(chunk[8+len(data):], crc32.ChecksumIEEE(chunk[4:8+len(data)]))
	original := append(append(append([]byte{}, encoded.Bytes()[:33]...), chunk...), encoded.Bytes()[33:]...)
	path, err := s.Artifact(v.ID, "images", original)
	if err != nil {
		t.Fatal(err)
	}
	export := filepath.Join(t.TempDir(), "history.md")
	if err = s.Export(v.ID, export); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(strings.TrimSuffix(lineage, string(filepath.Separator)), path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(export+".assets", rel))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("export rewrote binary image metadata")
	}
	if _, err = png.Decode(bytes.NewReader(got)); err != nil {
		t.Fatal("export invalid PNG", err)
	}
}

func TestChildToolLabelKeepsActorSeparateFromMarkdown(t *testing.T) {
	s, v, turn, request := historyFixture(t)
	actor := "main/child_a-_x_-bcdefghijk"
	call := provider.ToolCall{ID: "provider_child", Name: "read", Arguments: []byte(`{"path":"x"}`)}
	id, err := s.CallIntent(v.ID, turn, actor, request, call)
	if err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(`{"ok":true,"content":"text"}`)
	md := render.Tool("read", call.Arguments, result)
	entryID, err := s.CallResult(v.ID, turn, actor, id, result, nil, map[string]any{"result": result}, md, false)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.Entry(entryID)
	if err != nil {
		t.Fatal(err)
	}
	label, err := render.TerminalBriefing(s.Label(entry), 120, false)
	if err != nil || strings.Contains(label, actor) || entry.Actor != actor || !strings.Contains(label, "read") {
		t.Fatal(label, err)
	}
}

func historyFixture(t *testing.T) (*Store, Session, string, int64) {
	t.Helper()
	s, e := Open(filepath.Join(t.TempDir(), "data"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	model := provider.Selection{Provider: "script", Model: provider.ScriptModel(), Variant: "none"}
	v, turn := startHistorySession(t, s, t.TempDir(), model, "hello")
	req, e := s.StartRequest(v.ID, turn, "main", "coding", model)
	if e != nil {
		t.Fatal(e)
	}
	return s, v, turn, req
}

func startHistorySession(t *testing.T, s *Store, path string, model provider.Selection, text string) (Session, string) {
	t.Helper()
	id := NewID("session")
	turn, _, err := s.StartSession(id, path, model, provider.Message{Role: "user", Content: text})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.Session(id)
	if err != nil {
		t.Fatal(err)
	}
	return saved, turn
}

func TestStartSessionRollsBackFirstTurnAndMessageFailures(t *testing.T) {
	for _, table := range []string{"turns", "entries"} {
		t.Run(table, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "data"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			if _, err := s.DB.Exec("CREATE TRIGGER reject_first BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(ABORT,'injected first-save failure'); END"); err != nil {
				t.Fatal(err)
			}
			id, path := NewID("session"), t.TempDir()
			model := provider.Selection{Provider: "script", Model: provider.ScriptModel(), Variant: "none"}
			if _, _, err := s.StartSession(id, path, model, provider.Message{Role: "user", Content: "first message"}); err == nil || !strings.Contains(err.Error(), "injected first-save failure") {
				t.Fatal(err)
			}
			for _, table := range []string{"workspaces", "sessions", "turns", "entries"} {
				var count int
				if err := s.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatal("partial first save", table, count, err)
				}
			}
			if _, err := s.DB.Exec("DROP TRIGGER reject_first"); err != nil {
				t.Fatal(err)
			}
			turn, entry, err := s.StartSession(id, path, model, provider.Message{Role: "user", Content: "first message"})
			if err != nil || turn == "" || entry == 0 {
				t.Fatal("same blank identity could not retry", turn, entry, err)
			}
			saved, err := s.Session(id)
			if err != nil || saved.ID != id || saved.EntryTip != entry {
				t.Fatal(saved, err)
			}
			var start, tip sql.NullInt64
			if err := s.DB.QueryRow("SELECT start_entry_id,start_file_tip_id FROM turns WHERE id=?", turn).Scan(&start, &tip); err != nil || start.Valid || tip.Valid {
				t.Fatal("first turn lost empty undo boundary", start, tip, err)
			}
		})
	}
}
func TestHistoryImmutableResultsBranchAndSystemInspection(t *testing.T) {
	s, v, turn, req := historyFixture(t)
	call := provider.ToolCall{ID: "provider_call", Name: "read", Arguments: json.RawMessage(`{"path":"x"}`)}
	id, e := s.CallIntent(v.ID, turn, "main", req, call)
	if e != nil {
		t.Fatal(e)
	}
	result := json.RawMessage(`{"ok":true,"content":"x"}`)
	md := render.Tool("read", call.Arguments, result)
	if _, e = s.CallResult(v.ID, turn, "main", id, result, nil, map[string]any{"result": result}, md, true); e != nil {
		t.Fatal(e)
	}
	if _, e = s.CallResult(v.ID, turn, "main", id, []byte(`{"ok":false}`), nil, nil, md, true); e == nil {
		t.Fatal("mutable result")
	}
	promptID, e := s.RecordSystemPrompt(v.ID, turn, "main", req, "exact system instruction")
	if e != nil {
		t.Fatal(e)
	}
	entry, e := s.Entry(promptID)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(s.Label(entry), "exact system") {
		t.Fatal("prompt exposed in placeholder")
	}
	text, e := s.Inspect(entry)
	if e != nil || text != "exact system instruction" {
		t.Fatal(text, e)
	}
	messages, e := s.Messages(v.ID)
	if e != nil {
		t.Fatal(e)
	}
	if len(messages) != 2 {
		t.Fatal(messages)
	}
	export := filepath.Join(t.TempDir(), "history.md")
	if e = s.Export(v.ID, export); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(export)
	if strings.Contains(string(b), "exact system instruction") {
		t.Fatal("export exposed system prompt")
	}
	exact, err := os.ReadFile(export + ".jsonl")
	if err != nil || !strings.Contains(string(exact), "exact system instruction") {
		t.Fatal("missing exact sidecar", err)
	}
	if e = s.Export(v.ID, export); e == nil {
		t.Fatal("overwrote export")
	}
	if e = s.Ping(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func TestContinuationRebasesRetainedUndoAndValidatesArchive(t *testing.T) {
	s, v, first, req := historyFixture(t)
	s.FinishRequest(req, "completed", nil)
	s.FinishTurn(first, "completed")
	s.Append(v.ID, first, "main", "message", "assistant", true, provider.Message{Role: "assistant", Content: "old"})
	second, e := s.BeginTurn(v.ID, "user", v.Model)
	if e != nil {
		t.Fatal(e)
	}
	retained, e := s.Append(v.ID, second, "main", "message", "user", true, provider.Message{Role: "user", Content: "retain this"})
	if e != nil {
		t.Fatal(e)
	}
	s.Append(v.ID, second, "main", "message", "assistant", true, provider.Message{Role: "assistant", Content: "reply", State: &provider.ReplayState{Provider: "script", Model: "scripted", Version: 1, Items: []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"old"}`)}}})
	s.FinishTurn(second, "completed")
	archive, e := s.ArchiveTranscript(v.ID, 0)
	if e != nil {
		t.Fatal(e)
	}
	latest := v.Model
	latest.Model.ID = "latest-choice"
	if e = s.SaveSelection(latest); e != nil {
		t.Fatal(e)
	}
	continuation, e := s.Continue(v.ID, "summary", archive, retained, nil, time.Now(), nil)
	if e != nil {
		t.Fatal(e)
	}
	choice, e := s.LastSelection(v.Model.Provider)
	if e != nil || choice == nil || choice.Model.ID != latest.Model.ID {
		t.Fatal("compaction replaced a later model choice", choice, e)
	}
	if e = s.SaveSelection(continuation.Model); e != nil {
		t.Fatal(e)
	}
	choice, e = s.LastSelection(v.Model.Provider)
	if e != nil || choice == nil || choice.Model.ID != v.Model.Model.ID {
		t.Fatal("continuation lost its model", choice, e)
	}
	target, e := s.UndoTarget(continuation.ID)
	if e != nil || target.EntryTip != continuation.UndoFloor {
		t.Fatal(target, e)
	}
	messages, e := s.Messages(continuation.ID)
	if e != nil || len(messages) != 3 || messages[2].State != nil {
		t.Fatal(messages, e)
	}
	old, e := s.Session(v.ID)
	if e != nil || !old.ReadOnly {
		t.Fatal(old, e)
	}
	os.Remove(archive)
	if _, e = s.Load(continuation.ID); e == nil {
		t.Fatal("accepted missing archive")
	}
}
