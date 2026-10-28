package history

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func TestLoadCopiesImmutableEntriesAndAdvancesCursorOnce(t *testing.T) {
	s, source, turn, request := historyFixture(t)
	call, err := s.CallIntent(source.ID, turn, "main", request, llm.ToolCall{ID: "synthetic", Name: "write", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitChange(source.ID, call, []string{"synthetic.txt"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE entries SET delivered_request_id=? WHERE session_id=?", request, source.ID); err != nil {
		t.Fatal(err)
	}
	for _, message := range []llm.Message{
		{Type: llm.TypeAssistant, Content: "historical assistant"},
		{Type: llm.TypeRuntimeInstruction, Content: "old environment"},
	} {
		if _, err := s.Append(source.ID, turn, "main", "message", message.Role(), true, message); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Append(source.ID, turn, "main/child_synthetic", "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: strings.Repeat("audit", 4096)}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Branch(source.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	// A copy is append-only. Neither old nor newly copied entry timestamps need
	// a follow-up UPDATE; the destination cursor should change once per prefix.
	if _, err := s.db.Exec(`CREATE TABLE load_cursor_updates(id INTEGER);
 CREATE TRIGGER load_no_entry_updates BEFORE UPDATE ON entries BEGIN SELECT RAISE(ABORT,'entry update during load'); END;
 CREATE TRIGGER load_cursor_update AFTER UPDATE OF active_entry_id ON sessions BEGIN INSERT INTO load_cursor_updates VALUES(new.active_entry_id); END;`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		loaded, err := s.Load(source.ID)
		if err != nil {
			t.Fatal(err)
		}
		after, err := s.Branch(loaded.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) || loaded.UndoFloor != loaded.EntryTip || loaded.FileTip != 0 {
			t.Fatal("invalid imported baseline", loaded, len(after), len(before))
		}
		var parent int64
		for i, got := range after {
			want := before[i]
			visible := want.Visible
			if want.Kind == "message" {
				var message llm.Message
				if err := json.Unmarshal(want.Content, &message); err != nil {
					t.Fatal(err)
				}
				if message.Type.IsRuntime() {
					visible = false
				}
			}
			if got.Parent != parent || got.Source != want.EventSeq() || got.Visible != visible || got.CreatedMS != want.CreatedMS || string(got.Content) != string(want.Content) || got.TurnID != want.TurnID || got.MainTurnID != want.MainTurnID || got.UndoOwnerTurnID != want.UndoOwnerTurnID || got.FileTip != 0 || got.Actor != want.Actor || got.Kind != want.Kind || got.Role != want.Role {
				t.Fatalf("copy changed historical evidence: got %+v, want %+v", got, want)
			}
			parent = got.ID
		}
		var acknowledged int
		if err := s.db.QueryRow("SELECT count(*) FROM entries WHERE session_id=? AND delivered_request_id IS NOT NULL", loaded.ID).Scan(&acknowledged); err != nil || acknowledged != 0 {
			t.Fatal("copy retained delivery acknowledgments", acknowledged, err)
		}
		// The second load must resolve source identities and ownership from original
		// rows, even when an intermediate copy has different indexed ownership.
		if _, err := s.db.Exec(`DROP TRIGGER load_no_entry_updates;
  UPDATE entries SET main_turn_id=NULL,undo_owner_turn_id=NULL WHERE session_id=?;
  CREATE TRIGGER load_no_entry_updates BEFORE UPDATE ON entries BEGIN SELECT RAISE(ABORT,'entry update during load'); END;`, loaded.ID); err != nil {
			t.Fatal(err)
		}
		source = loaded
	}
	var updates int
	if err := s.db.QueryRow("SELECT count(*) FROM load_cursor_updates").Scan(&updates); err != nil || updates != 2 {
		t.Fatal("cursor was updated per entry", updates, err)
	}
	unchanged, err := s.Branch(before[0].SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(unchanged, before) {
		t.Fatal("load changed source history")
	}
}

func TestLoadRollsBackMalformedCanonicalCopy(t *testing.T) {
	for _, body := range []string{`{"type":"assistant","content":123}`, `{"type":"user_input","content":"wrong indexed role"}`} {
		t.Run(body, func(t *testing.T) {
			s, source, turn, _ := historyFixture(t)
			entry, err := s.Append(source.ID, turn, "main/child_synthetic", "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: "valid before corruption"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("UPDATE entries SET content_json=? WHERE id=?", body, entry); err != nil {
				t.Fatal(err)
			}
			var sessionsBefore, entriesBefore int
			if err := s.db.QueryRow("SELECT count(*) FROM sessions").Scan(&sessionsBefore); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow("SELECT count(*) FROM entries").Scan(&entriesBefore); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Load(source.ID); err == nil {
				t.Fatal("malformed canonical copy was accepted")
			}
			var sessionsAfter, entriesAfter int
			if err := s.db.QueryRow("SELECT count(*) FROM sessions").Scan(&sessionsAfter); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow("SELECT count(*) FROM entries").Scan(&entriesAfter); err != nil {
				t.Fatal(err)
			}
			if sessionsBefore != sessionsAfter || entriesBefore != entriesAfter {
				t.Fatal("failed load left partial copy", sessionsBefore, sessionsAfter, entriesBefore, entriesAfter)
			}
		})
	}
}

func TestStreamItemsUsesAncestryPrimaryKeyLookups(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+streamItemsSQL, v.EntryTip)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
		indexed = indexed || strings.Contains(detail, "SEARCH e USING INTEGER PRIMARY KEY")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, detail := range plan {
		if strings.Contains(detail, "SCAN e") || strings.Contains(detail, "AUTOMATIC") {
			t.Fatalf("stream replay must seek ancestry IDs, not scan unrelated entries:\n%s", strings.Join(plan, "\n"))
		}
	}
	if !indexed {
		t.Fatalf("no entry primary-key acquisition:\n%s", strings.Join(plan, "\n"))
	}
}

// BenchmarkLoad exercises independent writable snapshots, including large hidden
// audit messages. All history is synthetic and local to the benchmark directory.
func BenchmarkLoad(b *testing.B) {
	for _, n := range []int{100, 1000} {
		for _, hiddenBytes := range []int{0, 16 << 10} {
			b.Run(fmt.Sprintf("entries=%d/hidden=%d", n, hiddenBytes), func(b *testing.B) {
				s, err := Open(filepath.Join(b.TempDir(), "history"))
				if err != nil {
					b.Fatal(err)
				}
				defer s.Close()
				source := NewID("session")
				model := llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}
				if _, _, err := s.StartSession(source, b.TempDir(), model, llm.Message{Type: llm.TypeUserInput, Content: "synthetic"}); err != nil {
					b.Fatal(err)
				}
				payload, err := json.Marshal(llm.Message{Type: llm.TypeAssistant, Content: strings.Repeat("x", hiddenBytes)})
				if err != nil {
					b.Fatal(err)
				}
				err = s.transact(func(tx *sql.Tx) error {
					for range n {
						if _, err := appendTx(tx, source, "", "main", "message", "assistant", hiddenBytes == 0, payload, 0); err != nil {
							return err
						}
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if _, err := s.Load(source); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
