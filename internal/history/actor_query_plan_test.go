package history

import (
	"strings"
	"testing"

	"ttc/internal/llm"
)

func TestActorBranchQueryStaysAncestryDrivenAfterAnalyze(t *testing.T) {
	s, selected, turn, _ := historyFixture(t)
	for i := 0; i < 8; i++ {
		if _, err := s.AppendMessage(selected.ID, turn, "main", llm.Message{Type: llm.TypeAssistant, Content: "Selected evidence"}); err != nil {
			t.Fatal(err)
		}
	}
	cut, err := s.Session(selected.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := startHistorySession(t, s, t.TempDir(), selected.Model, "Unrelated history")
	// A small ancestry in a large shared table must drive body acquisition even
	// when SQLite has statistics favoring a covering scan of global source IDs.
	if _, err := s.db.Exec(`WITH RECURSIVE n(i) AS (
		VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<100000
	) INSERT INTO entries(session_id,actor_id,kind,role,model_visible,content_json,created_ms)
		SELECT ?,'main','message','assistant',1,'{"type":"assistant","content":"unrelated"}',1 FROM n`, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("ANALYZE"); err != nil {
		t.Fatal(err)
	}
	for _, boundariesOnly := range []bool{false, true} {
		rows, err := s.db.Query("EXPLAIN QUERY PLAN "+actorBranchQuery, cut.EntryTip, "main", boundariesOnly)
		if err != nil {
			t.Fatal(err)
		}
		var details []string
		selectedPK, originPK, globalScan := false, false, false
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			details = append(details, detail)
			selectedPK = selectedPK || strings.Contains(detail, "SEARCH selected USING INTEGER PRIMARY KEY")
			originPK = originPK || strings.Contains(detail, "SEARCH e USING INTEGER PRIMARY KEY")
			globalScan = globalScan || strings.HasPrefix(detail, "SCAN selected") || strings.HasPrefix(detail, "SCAN e ") || detail == "SCAN e"
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if globalScan || !selectedPK || !originPK {
			t.Fatalf("boundariesOnly=%t: selected ancestry must drive PK acquisition, not global entry scans:\n%s", boundariesOnly, strings.Join(details, "\n"))
		}
	}
	entries, err := actorBranchWith(s.db, selected.ID, cut.EntryTip, "main", false)
	if err != nil || len(entries) != 10 {
		t.Fatal("actor acquisition changed selected evidence", len(entries), err)
	}
	for _, entry := range entries {
		if entry.SessionID != selected.ID || entry.ID > cut.EntryTip {
			t.Fatal("unrelated history entered actor transcript", entry)
		}
	}
}
