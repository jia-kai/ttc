package history

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"ttc/internal/llm"
)

func TestRecoveryInstructionQueryExcludesLargeUnrelatedBodies(t *testing.T) {
	s, saved, turn, _ := historyFixture(t)
	large := strings.Repeat("x", 4<<20)
	for _, entry := range []struct {
		actor, kind, role string
		content           any
	}{
		{"main", "status", "", map[string]string{"type": "large_audit", "data": large}},
		{"main", "message", "assistant", llm.Message{Type: llm.TypeAssistant, Content: large}},
		{"main/child_other", "message", "developer", llm.Message{Type: llm.TypeRuntimeInstruction, Content: large}},
	} {
		if _, err := s.Append(saved.ID, turn, entry.actor, entry.kind, entry.role, false, entry.content); err != nil {
			t.Fatal(err)
		}
	}
	// encoding/json accepts case-insensitive canonical field names. Candidate
	// selection must not silently filter these through case-sensitive json_extract.
	warning := llm.Message{Type: llm.TypeRuntimeInstruction, Content: "Recovery warning", RequestID: 42}
	id, err := s.Append(saved.ID, turn, "main", "message", "developer", true,
		json.RawMessage(`{"TYPE":"runtime_instruction","CONTENT":"Recovery warning","REQUEST_ID":42}`))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query(recoveryInstructionQuery, saved.ID, "main", warning.Role())
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for rows.Next() {
		var candidate, createdMS int64
		var data string
		if err := rows.Scan(&candidate, &createdMS, &data); err != nil {
			t.Fatal(err)
		}
		if candidate != id || len(data) > 512 {
			t.Fatalf("unrelated payload acquired: id=%d bytes=%d", candidate, len(data))
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("unexpected recovery candidates", count)
	}
	plan, err := s.db.Query("EXPLAIN QUERY PLAN "+recoveryInstructionQuery, saved.ID, "main", warning.Role())
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	var details []string
	for plan.Next() {
		var id, parent, unused int
		var detail string
		if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := plan.Err(); err != nil {
		t.Fatal(err)
	}
	if err := plan.Close(); err != nil {
		t.Fatal(err)
	}
	detail := strings.Join(details, "\n")
	if !strings.Contains(detail, "SEARCH e USING INTEGER PRIMARY KEY") || strings.Contains(detail, "SCAN e") {
		t.Fatal("recovery body acquisition is not ancestry-driven by primary key", detail)
	}
	committed, added, err := s.EnsureRecoveryInstruction(saved.ID, turn, "main", warning)
	if err != nil || added || committed.EntryID != id || !reflect.DeepEqual(committed.Message, warning) {
		t.Fatal("case-insensitive recovery identity was not preserved", committed, added, err)
	}
	// Matching envelope candidates still pass through the canonical decoder,
	// including its errors, rather than being silently excluded by a JSON predicate.
	if _, err := s.db.Exec("UPDATE entries SET content_json=? WHERE id=?", `{"type":"runtime_instruction","request_id":"invalid"}`, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnsureRecoveryInstruction(saved.ID, turn, "main", warning); err == nil {
		t.Fatal("malformed matching candidate was silently ignored")
	}
}

func TestEnsureRecoveryInstructionConcurrentDeduplication(t *testing.T) {
	s, saved, turn, _ := historyFixture(t)
	warning := llm.Message{Type: llm.TypeRuntimeInstruction, RequestID: 42, Content: "Partial output recovery"}
	const workers = 12
	var wg sync.WaitGroup
	committed := make([]CommittedMessage, workers)
	added := make([]bool, workers)
	errors := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			committed[i], added[i], errors[i] = s.EnsureRecoveryInstruction(saved.ID, turn, "main", warning)
		}()
	}
	wg.Wait()
	insertions := 0
	for i := range workers {
		if errors[i] != nil || committed[i].EntryID == 0 || committed[i].EntryID != committed[0].EntryID || committed[i].CreatedMS <= 0 || !reflect.DeepEqual(committed[i].Message, warning) {
			t.Fatalf("worker %d: %+v, %v", i, committed[i], errors[i])
		}
		if added[i] {
			insertions++
		}
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM entries WHERE session_id=? AND kind='message' AND role='developer'", saved.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if insertions != 1 || count != 1 {
		t.Fatalf("recovery inserted %d times with %d matching rows", insertions, count)
	}
}

func TestEnsureRecoveryInstructionScopesBranchActorAndRequest(t *testing.T) {
	s, saved, turn, _ := historyFixture(t)
	warning := llm.Message{Type: llm.TypeRuntimeInstruction, RequestID: 42, Content: "Partial output recovery"}
	first, added, err := s.EnsureRecoveryInstruction(saved.ID, turn, "main", warning)
	if err != nil || !added {
		t.Fatal(first, added, err)
	}
	plan, err := s.PlanBranch(saved.ID, saved.EntryTip)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CommitRestore(plan); err != nil {
		t.Fatal(err)
	}
	second, added, err := s.EnsureRecoveryInstruction(saved.ID, turn, "main", warning)
	if err != nil || !added || second.EntryID == first.EntryID {
		t.Fatal("an instruction on a sibling branch prevented recovery", second, added, err)
	}
	for _, actor := range []string{"main/child_recovery", "main/btw_recovery"} {
		child, added, err := s.EnsureRecoveryInstruction(saved.ID, "", actor, warning)
		if err != nil || !added || child.EntryID == second.EntryID {
			t.Fatal("another actor's instruction prevented recovery", child, added, err)
		}
		if duplicate, added, err := s.EnsureRecoveryInstruction(saved.ID, "", actor, warning); err != nil || added || duplicate.EntryID != child.EntryID {
			t.Fatal("actor recovery was not deduplicated", duplicate, added, err)
		}
	}
	warning.RequestID++
	if _, added, err := s.EnsureRecoveryInstruction(saved.ID, turn, "main", warning); err != nil || !added {
		t.Fatal("a distinct request's recovery was deduplicated", added, err)
	}
	messages, err := s.Messages(saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	warnings := 0
	for _, message := range messages {
		if message.Type == llm.TypeRuntimeInstruction && message.Content == warning.Content {
			warnings++
		}
	}
	if warnings != 2 {
		t.Fatalf("main input contains %d recovery instructions; child/aside evidence leaked", warnings)
	}
}

func TestEnsureRecoveryInstructionRollbackAndValidation(t *testing.T) {
	s, saved, turn, _ := historyFixture(t)
	warning := llm.Message{Type: llm.TypeRuntimeInstruction, Content: "Recovery warning"}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_recovery_instruction BEFORE UPDATE OF active_entry_id ON sessions BEGIN SELECT RAISE(ABORT,'forced recovery failure'); END`); err != nil {
		t.Fatal(err)
	}
	committed, added, err := s.EnsureRecoveryInstruction(saved.ID, turn, "main", warning)
	if err == nil || added || !reflect.DeepEqual(committed, CommittedMessage{}) {
		t.Fatal("rolled-back recovery identity escaped", committed, added, err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM entries WHERE session_id=? AND kind='message' AND role='developer'", saved.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed recovery left an instruction", count, err)
	}
	for _, invalid := range []llm.Message{{}, {Type: llm.TypeUserInput, Content: "Not a recovery instruction"}, {Type: llm.TypeRuntimeInstruction}} {
		if _, added, err := s.EnsureRecoveryInstruction(saved.ID, turn, "main", invalid); err == nil || added {
			t.Fatal("accepted invalid recovery instruction", invalid, added, err)
		}
	}
}
