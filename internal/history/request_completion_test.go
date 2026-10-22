package history

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"

	"ttc/internal/llm"
)

func TestRequestCompletionUsage(t *testing.T) {
	first := &llm.Usage{InputTokens: 10, OutputTokens: 2}
	last := &llm.Usage{InputTokens: 20, OutputTokens: 3}
	for _, tc := range []struct {
		name       string
		completion RequestCompletion
		want       *llm.Usage
	}{
		{"last successful", RequestCompletion{Status: "completed", Attempts: []RequestAttempt{{Status: "failed", Usage: first}, {Status: "completed", Usage: last}}}, last},
		{"last success absent", RequestCompletion{Status: "completed", Attempts: []RequestAttempt{{Status: "completed", Usage: first}, {Status: "completed"}}}, nil},
		{"failed request", RequestCompletion{Status: "failed", Attempts: []RequestAttempt{{Status: "completed", Usage: first}}}, nil},
		{"failed attempt", RequestCompletion{Status: "completed", Attempts: []RequestAttempt{{Status: "failed", Usage: first}}}, nil},
		{"unreported status", RequestCompletion{Status: "completed", Attempts: []RequestAttempt{{Usage: first}}}, nil},
		{"no attempts", RequestCompletion{Status: "completed"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.completion.usage(); got != tc.want {
				t.Fatalf("usage = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestFinishRequestImmutableObservations(t *testing.T) {
	s, v, turn, request := historyFixture(t)
	var admission int64
	if err := s.db.QueryRow("SELECT admission_entry_id FROM model_requests WHERE id=?", request).Scan(&admission); err != nil {
		t.Fatal(err)
	}
	prompt, err := s.RecordSystemPrompt(v.ID, turn, "main", request, "first exact instructions")
	if err != nil {
		t.Fatal(err)
	}
	first := RequestCompletion{Status: "completed", Attempts: []RequestAttempt{
		{Status: "failed", Attempt: 1, Error: "retry", Usage: &llm.Usage{InputTokens: 99}},
		{Status: "completed", Attempt: 2, DurationMS: 25, ResponseID: "response", Usage: &llm.Usage{InputTokens: 12, OutputTokens: 4}},
	}}
	if err := s.FinishRequest(request, first); err != nil {
		t.Fatal(err)
	}
	// A later prompt and terminal outcome must not rewrite the first evidence.
	if _, err := s.RecordSystemPrompt(v.ID, turn, "main", request, "later instructions"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRequest(request, RequestCompletion{Status: "failed", Attempts: []RequestAttempt{{Status: "failed", Usage: &llm.Usage{InputTokens: 100}}}}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query(`SELECT o.admission_entry_id,o.system_prompt_entry_id,o.status,o.usage_json,e.content_json
		FROM request_observations o JOIN entries e ON e.id=o.completion_entry_id WHERE o.request_id=? ORDER BY o.completion_entry_id`, request)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var gotAdmission, gotPrompt int64
		var status, event string
		var usage sql.NullString
		if err := rows.Scan(&gotAdmission, &gotPrompt, &status, &usage, &event); err != nil {
			t.Fatal(err)
		}
		if gotAdmission != admission {
			t.Fatalf("admission = %d, want original %d", gotAdmission, admission)
		}
		if count == 0 {
			if gotPrompt != prompt || status != "completed" || !usage.Valid {
				t.Fatalf("first observation: prompt=%d status=%s usage=%v", gotPrompt, status, usage)
			}
			var tokens llm.Usage
			if err := json.Unmarshal([]byte(usage.String), &tokens); err != nil || tokens.InputTokens != 12 {
				t.Fatalf("usage = %s: %v", usage.String, err)
			}
			var audit struct{ RequestCompletion }
			if err := json.Unmarshal([]byte(event), &audit); err != nil || !reflect.DeepEqual(audit.RequestCompletion, first) {
				t.Fatalf("audit = %s: %v", event, err)
			}
		} else if status != "failed" || usage.Valid || gotPrompt == prompt {
			t.Fatalf("second observation: prompt=%d status=%s usage=%v", gotPrompt, status, usage)
		}
		count++
	}
	if err := rows.Err(); err != nil || count != 2 {
		t.Fatalf("observations=%d: %v", count, err)
	}
}

func TestFinishRequestRejectsInvalidCompletionAtomically(t *testing.T) {
	s, _, _, request := historyFixture(t)
	for _, completion := range []RequestCompletion{
		{Status: "running"},
		{Status: "completed", Attempts: []RequestAttempt{{Status: "unknown"}}},
		{Status: "completed", Attempts: []RequestAttempt{{DurationMS: -1}}},
		{Status: "completed", Attempts: []RequestAttempt{{Attempt: -1}}},
	} {
		if err := s.FinishRequest(request, completion); err == nil {
			t.Fatalf("accepted %+v", completion)
		}
	}
	var status string
	var count int
	if err := s.db.QueryRow("SELECT status FROM model_requests WHERE id=?", request).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM request_observations WHERE request_id=?", request).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if status != "running" || count != 0 {
		t.Fatalf("invalid completion changed request: status=%s observations=%d", status, count)
	}
}

func TestFinishRequestRetainsUnusableUsageAudit(t *testing.T) {
	negative, excessive := -1, 20
	for _, tc := range []struct {
		name     string
		usage    llm.Usage
		measured bool
	}{
		{"negative input", llm.Usage{InputTokens: -1, OutputTokens: 2}, false},
		{"negative output", llm.Usage{InputTokens: 10, OutputTokens: -1}, false},
		{"optional counters", llm.Usage{InputTokens: 10, OutputTokens: 2, CachedInputTokens: &excessive, CacheWriteTokens: &negative, ReasoningOutputTokens: &excessive}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, request := historyFixture(t)
			completion := RequestCompletion{Status: "completed", Attempts: []RequestAttempt{
				{Status: "completed", Usage: &llm.Usage{InputTokens: 5}},
				{Status: "completed", Usage: &tc.usage},
			}}
			if err := s.FinishRequest(request, completion); err != nil {
				t.Fatal(err)
			}
			var status, event string
			var usage sql.NullString
			if err := s.db.QueryRow(`SELECT r.status,e.content_json,o.usage_json
				FROM model_requests r JOIN request_observations o ON o.request_id=r.id
				JOIN entries e ON e.id=o.completion_entry_id WHERE r.id=?`, request).Scan(&status, &event, &usage); err != nil {
				t.Fatal(err)
			}
			var audit struct{ RequestCompletion }
			if err := json.Unmarshal([]byte(event), &audit); err != nil {
				t.Fatal(err)
			}
			if status != "completed" || !reflect.DeepEqual(audit.RequestCompletion, completion) || usage.Valid != tc.measured {
				t.Fatalf("completion evidence changed: status=%s audit=%+v normalized=%v", status, audit, usage)
			}
			if tc.measured {
				var measured llm.Usage
				if err := json.Unmarshal([]byte(usage.String), &measured); err != nil || !reflect.DeepEqual(measured, tc.usage) {
					t.Fatalf("normalized optional counters changed: %s: %v", usage.String, err)
				}
			}
		})
	}
}
