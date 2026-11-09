package history

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"ttc/internal/llm"
)

func usageFixture(t *testing.T) (*Store, Session, string) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	model := llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}
	v, turn := startHistorySession(t, s, t.TempDir(), model, "original input")
	return s, v, turn
}

func usageCheckpoint(messages []llm.Message) llm.TokenCheckpoint {
	// Persistence tests need stable evidence, not a provider's counting algorithm.
	return llm.TokenCheckpoint{
		Version: 2, LocalInput: len(messages),
		Compatible: strings.Repeat("a", 64), Exact: strings.Repeat("b", 64),
	}
}

func admitUsage(t *testing.T, s *Store, v Session, turn string, checkpoint bool) Admission {
	t.Helper()
	var saved *llm.TokenCheckpoint
	runtimeContext := &llm.Message{Type: llm.TypeRuntimeContext, Content: `{"type":"runtime_context","original":true}`}
	if checkpoint {
		messages, err := s.Messages(v.ID)
		if err != nil {
			t.Fatal(err)
		}
		value := usageCheckpoint(append(messages, *runtimeContext))
		saved = &value
	}
	admitted, err := s.AdmitRequest(RequestAdmission{SessionID: v.ID, TurnID: turn, Actor: "main", Selection: v.Model, Checkpoint: saved, Context: runtimeContext})
	if err != nil {
		t.Fatal(err)
	}
	return admitted
}

func finishUsage(t *testing.T, s *Store, request int64, status string, tokens *llm.Usage) {
	t.Helper()
	if err := s.FinishRequest(request, RequestCompletion{Status: status, Attempts: []RequestAttempt{{Status: status, Usage: tokens}}}); err != nil {
		t.Fatal(err)
	}
}

func readUsage(t *testing.T, s *Store, session string) MainUsage {
	t.Helper()
	usage, err := s.MainUsage(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	return usage
}

func TestMainUsageEmptyMissingAndCancellation(t *testing.T) {
	s, v, _ := usageFixture(t)
	if got := readUsage(t, s, v.ID); got.Reported != nil || got.Anchor != nil {
		t.Fatal(got)
	}
	if _, err := s.MainUsage(context.Background(), "missing"); err == nil {
		t.Fatal("missing session accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.MainUsage(ctx, v.ID); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not propagated", err)
	}
}

func TestMainUsageCheckpointAndImmutableCompletion(t *testing.T) {
	s, v, turn := usageFixture(t)
	admitted := admitUsage(t, s, v, turn, true)
	cached, written, reasoning := 40, 5, 3
	tokens := llm.Usage{InputTokens: 123, OutputTokens: 17, CachedInputTokens: &cached, CacheWriteTokens: &written, ReasoningOutputTokens: &reasoning}
	finishUsage(t, s, admitted.RequestID, "completed", &tokens)
	// Mutable request status must not override the selected immutable observation.
	if _, err := s.db.Exec(`UPDATE model_requests SET status='failed' WHERE id=?`, admitted.RequestID); err != nil {
		t.Fatal(err)
	}
	got := readUsage(t, s, v.ID)
	if got.Reported == nil || got.Anchor == nil || got.Reported.RequestID != admitted.RequestID || got.Anchor.RequestID != admitted.RequestID || !reflect.DeepEqual(got.Anchor.Tokens, tokens) || !reflect.DeepEqual(got.Anchor.Selection, v.Model) {
		t.Fatal(got)
	}
	want := usageCheckpoint(admitted.Messages)
	if got.Anchor.Checkpoint == nil || *got.Anchor.Checkpoint != want {
		t.Fatal("checkpoint anchor differs from admitted evidence", got.Anchor)
	}
}

func TestMainUsageSharedForksPreserveLegacyUsageWithoutCheckpoint(t *testing.T) {
	s, original, turn := usageFixture(t)
	admitted := admitUsage(t, s, original, turn, false)
	const system = "Exact original system instructions.\n"
	if _, err := s.RecordSystemPrompt(original.ID, turn, "main", admitted.RequestID, system); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Assistant(original.ID, turn, "main", admitted.RequestID, llm.Message{Type: llm.TypeAssistant, Content: "response excluded from original input"}); err != nil {
		t.Fatal(err)
	}
	finishUsage(t, s, admitted.RequestID, "completed", &llm.Usage{InputTokens: 210, OutputTokens: 12})
	loaded := original
	for range 2 {
		var err error
		loaded, err = s.Load(loaded.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	// Source advancement and replacement table diagnostics must not change a fork.
	later := admitUsage(t, s, original, turn, true)
	finishUsage(t, s, later.RequestID, "completed", &llm.Usage{InputTokens: 999})
	got := readUsage(t, s, loaded.ID)
	if got.Reported == nil || got.Anchor == nil || got.Reported.RequestID != admitted.RequestID || got.Anchor.RequestID != admitted.RequestID || got.Anchor.Checkpoint != nil || got.Reported.Tokens.InputTokens != 210 {
		t.Fatal("shared fork lost original source-backed legacy usage", got)
	}
}

func TestMainUsageSharedForkDoesNotAcquireLaterSourceCompletion(t *testing.T) {
	s, original, turn := usageFixture(t)
	first := admitUsage(t, s, original, turn, true)
	finishUsage(t, s, first.RequestID, "completed", &llm.Usage{InputTokens: 100})
	pending := admitUsage(t, s, original, turn, true)
	loaded, err := s.Load(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	finishUsage(t, s, pending.RequestID, "completed", &llm.Usage{InputTokens: 999})
	got := readUsage(t, s, loaded.ID)
	if got.Reported == nil || got.Anchor == nil || got.Reported.RequestID != first.RequestID || got.Anchor.RequestID != first.RequestID {
		t.Fatal("shared fork acquired later source completion", got)
	}
}

func TestMainUsageAnchorRequiresSelectedOwnedAdmission(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, corruption := range []string{"unselected", "foreign owner"} {
			name := "legacy/"
			if checkpoint {
				name = "checkpoint/"
			}
			t.Run(name+corruption, func(t *testing.T) {
				s, v, turn := usageFixture(t)
				admitted := admitUsage(t, s, v, turn, checkpoint)
				finishUsage(t, s, admitted.RequestID, "completed", &llm.Usage{InputTokens: 100})
				other, otherTurn := startHistorySession(t, s, t.TempDir(), v.Model, "other input")
				foreign := admitUsage(t, s, other, otherTurn, checkpoint)
				if corruption == "unselected" {
					if _, err := s.db.Exec(`UPDATE request_observations SET admission_entry_id=? WHERE request_id=?`, foreign.EntryTip, admitted.RequestID); err != nil {
						t.Fatal(err)
					}
					got := readUsage(t, s, v.ID)
					if got.Reported == nil || got.Reported.RequestID != admitted.RequestID || got.Anchor != nil {
						t.Fatal("unselected admission supplied an accounting anchor", got)
					}
				} else {
					if _, err := s.db.Exec(`UPDATE model_requests SET session_id=? WHERE id=?`, other.ID, admitted.RequestID); err != nil {
						t.Fatal(err)
					}
					if _, err := s.MainUsage(context.Background(), v.ID); err == nil || !strings.Contains(err.Error(), "does not own request") {
						t.Fatal("foreign admission owner supplied an accounting anchor", err)
					}
				}
			})
		}
	}
}

func TestMainUsageSelectedBranchExcludesSiblingMeasurement(t *testing.T) {
	s, v, turn := usageFixture(t)
	first := admitUsage(t, s, v, turn, true)
	finishUsage(t, s, first.RequestID, "completed", &llm.Usage{InputTokens: 100})
	cut, err := s.Session(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	sibling := admitUsage(t, s, v, turn, true)
	finishUsage(t, s, sibling.RequestID, "completed", &llm.Usage{InputTokens: 999})
	if _, err := s.db.Exec("UPDATE sessions SET active_entry_id=? WHERE id=?", cut.EntryTip, v.ID); err != nil {
		t.Fatal(err)
	}
	failed := admitUsage(t, s, v, turn, true)
	finishUsage(t, s, failed.RequestID, "failed", &llm.Usage{InputTokens: 777})
	got := readUsage(t, s, v.ID)
	if got.Reported == nil || got.Anchor == nil || got.Reported.RequestID != first.RequestID || got.Anchor.RequestID != first.RequestID {
		t.Fatal("sibling or failed measurement selected", got)
	}
	loaded, err := s.Load(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := readUsage(t, s, loaded.ID); got.Anchor == nil || got.Anchor.RequestID != first.RequestID {
		t.Fatal("loaded branch selected sibling", got)
	}
}

func TestMainUsageNoUsageFailedAndExcludedRequests(t *testing.T) {
	s, v, turn := usageFixture(t)
	first := admitUsage(t, s, v, turn, true)
	finishUsage(t, s, first.RequestID, "completed", &llm.Usage{InputTokens: 100})
	noUsage := admitUsage(t, s, v, turn, true)
	// A failed attempt's usage cannot supply a successful response's measurement.
	if err := s.FinishRequest(noUsage.RequestID, RequestCompletion{Status: "completed", Attempts: []RequestAttempt{
		{Status: "failed", Usage: &llm.Usage{InputTokens: 888}},
		{Status: "completed"},
	}}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"failed", "interrupted"} {
		failed := admitUsage(t, s, v, turn, true)
		finishUsage(t, s, failed.RequestID, status, &llm.Usage{InputTokens: 999})
	}
	for _, test := range []struct{ actor, purpose string }{
		{"main", "naming"}, {"main", "compaction"}, {"main/child_usage", "coding"}, {"main/aside_usage", "coding"},
	} {
		request, err := s.StartRequest(v.ID, turn, test.actor, test.purpose, v.Model)
		if err != nil {
			t.Fatal(err)
		}
		finishUsage(t, s, request, "completed", &llm.Usage{InputTokens: 999})
	}
	_ = admitUsage(t, s, v, turn, true) // Running requests are not measurements.
	got := readUsage(t, s, v.ID)
	if got.Reported != nil || got.Anchor == nil || got.Anchor.RequestID != first.RequestID || got.Anchor.Tokens.InputTokens != 100 {
		t.Fatal("no-usage completion or excluded request corrupted main measurement", got)
	}
}

func TestMainUsageFinalSuccessfulAttemptWins(t *testing.T) {
	s, v, turn := usageFixture(t)
	admitted := admitUsage(t, s, v, turn, true)
	if err := s.FinishRequest(admitted.RequestID, RequestCompletion{Status: "completed", Attempts: []RequestAttempt{
		{Status: "completed", Usage: &llm.Usage{InputTokens: 1}},
		{Status: "completed", Usage: &llm.Usage{InputTokens: 2}},
	}}); err != nil {
		t.Fatal(err)
	}
	if got := readUsage(t, s, v.ID); got.Reported == nil || got.Reported.Tokens.InputTokens != 2 {
		t.Fatal(got)
	}
}

func TestMainUsageDoesNotDecodeAuditPayloads(t *testing.T) {
	s, v, turn := usageFixture(t)
	older := admitUsage(t, s, v, turn, true)
	finishUsage(t, s, older.RequestID, "completed", &llm.Usage{InputTokens: 100})
	// Observation reads must not decode audit attempts, including the latest
	// completion's diagnostics. Their typed evidence was committed atomically.
	if _, err := s.db.Exec(`UPDATE entries SET content_json=json_set(content_json,'$.attempts',json('["not-json"]'))
		WHERE kind='status' AND json_extract(content_json,'$.type')='request_finished'
		AND json_extract(content_json,'$.request_id')=?`, older.RequestID); err != nil {
		t.Fatal(err)
	}
	latest := admitUsage(t, s, v, turn, true)
	finishUsage(t, s, latest.RequestID, "completed", &llm.Usage{InputTokens: 200})
	if _, err := s.db.Exec(`UPDATE entries SET content_json=json_set(content_json,'$.attempts',json('["not-json"]'))
		WHERE id=(SELECT completion_entry_id FROM request_observations WHERE request_id=?)`, latest.RequestID); err != nil {
		t.Fatal(err)
	}
	got := readUsage(t, s, v.ID)
	if got.Reported == nil || got.Anchor == nil || got.Reported.RequestID != latest.RequestID || got.Anchor.RequestID != latest.RequestID {
		t.Fatal("latest measurement did not stop payload decoding", got)
	}
	noUsage := admitUsage(t, s, v, turn, true)
	finishUsage(t, s, noUsage.RequestID, "completed", nil)
	got = readUsage(t, s, v.ID)
	if got.Reported != nil || got.Anchor == nil || got.Anchor.RequestID != latest.RequestID {
		t.Fatal("no-usage suffix forced older payload acquisition", got)
	}
}

func TestMainUsageDistinguishesUnavailableInputFromMeasuredZero(t *testing.T) {
	for _, measured := range []bool{false, true} {
		name := "unavailable"
		if measured {
			name = "measured zero"
		}
		t.Run(name, func(t *testing.T) {
			s, v, turn := usageFixture(t)
			first := admitUsage(t, s, v, turn, true)
			finishUsage(t, s, first.RequestID, "completed", &llm.Usage{InputTokens: 100})
			latest := admitUsage(t, s, v, turn, true)
			var tokens *llm.Usage
			if measured {
				tokens = &llm.Usage{}
			}
			finishUsage(t, s, latest.RequestID, "completed", tokens)
			got := readUsage(t, s, v.ID)
			if measured {
				if got.Reported == nil || got.Anchor == nil || got.Reported.RequestID != latest.RequestID || got.Anchor.RequestID != latest.RequestID || got.Anchor.Tokens.InputTokens != 0 {
					t.Fatal("measured zero was treated as unavailable", got)
				}
			} else if got.Reported != nil || got.Anchor == nil || got.Anchor.RequestID != first.RequestID {
				t.Fatal("unavailable input was treated as measured zero", got)
			}
		})
	}
}

func TestMainUsageCompactionResetUsesAdmissionSource(t *testing.T) {
	s, original, turn := usageFixture(t)
	admitted := admitUsage(t, s, original, turn, true)
	finishUsage(t, s, admitted.RequestID, "completed", &llm.Usage{InputTokens: 100})
	if err := s.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	entries, err := s.Branch(original.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	cut := entries[len(entries)-1].ID
	// Copy both admission and completion after the new physical summary. Their
	// original source admission is still pre-compaction and cannot calibrate it.
	continued, err := s.continueFixture(original.ID, llm.NewCompactionContext("summary"), cut, entries[0].ID, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(continued.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{continued.ID, loaded.ID} {
		got := readUsage(t, s, id)
		if got.Reported == nil || got.Reported.RequestID != admitted.RequestID || got.Anchor != nil {
			t.Fatal("pre-compaction source admission calibrated copied tail", got)
		}
	}
	newTurn, err := s.BeginTurn(loaded.ID, "async", loaded.Model)
	if err != nil {
		t.Fatal(err)
	}
	post := admitUsage(t, s, loaded, newTurn, true)
	finishUsage(t, s, post.RequestID, "completed", &llm.Usage{InputTokens: 50})
	loadedAgain, err := s.Load(loaded.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := readUsage(t, s, loadedAgain.ID)
	if got.Anchor == nil || got.Anchor.RequestID != post.RequestID || got.Anchor.Tokens.InputTokens != 50 {
		t.Fatal("post-compaction anchor was rejected after copying summary", got)
	}
}

func TestMainUsageLegacyUsageNeedsNoPrompt(t *testing.T) {
	for _, missing := range []string{"record", "artifact"} {
		t.Run(missing, func(t *testing.T) {
			s, v, turn := usageFixture(t)
			admitted := admitUsage(t, s, v, turn, false)
			if missing == "artifact" {
				entry, err := s.RecordSystemPrompt(v.ID, turn, "main", admitted.RequestID, "instructions")
				if err != nil {
					t.Fatal(err)
				}
				view, err := s.ReadEntryView(context.Background(), entry)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(view.Prompt.Path); err != nil {
					t.Fatal(err)
				}
			}
			finishUsage(t, s, admitted.RequestID, "completed", &llm.Usage{InputTokens: 100})
			loaded, err := s.Load(v.ID)
			if err != nil {
				t.Fatal(err)
			}
			got := readUsage(t, s, loaded.ID)
			if got.Reported == nil || got.Anchor == nil || got.Reported.RequestID != admitted.RequestID || got.Anchor.RequestID != admitted.RequestID || got.Anchor.Checkpoint != nil || got.Reported.Tokens.InputTokens != 100 {
				t.Fatal("legacy usage depended on prompt reconstruction", got)
			}
		})
	}
}

func TestMainUsageLegacyDoesNotReadPromptArtifact(t *testing.T) {
	s, v, turn := usageFixture(t)
	admitted := admitUsage(t, s, v, turn, false)
	id, err := s.RecordSystemPrompt(v.ID, turn, "main", admitted.RequestID, "instructions")
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.ReadEntryView(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(view.Prompt.Path, []byte(strings.Repeat("x", instructionSnapshotBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	finishUsage(t, s, admitted.RequestID, "completed", &llm.Usage{InputTokens: 100})
	got := readUsage(t, s, v.ID)
	if got.Reported == nil || got.Anchor == nil || got.Reported.Tokens.InputTokens != 100 || got.Anchor.Checkpoint != nil {
		t.Fatal("legacy usage depended on prompt artifact contents", got)
	}
}

func TestMainUsageRejectsCorruptCheckpoint(t *testing.T) {
	s, v, turn := usageFixture(t)
	admitted := admitUsage(t, s, v, turn, true)
	finishUsage(t, s, admitted.RequestID, "completed", &llm.Usage{InputTokens: 100})
	if _, err := s.db.Exec(`UPDATE request_observations SET checkpoint_json=json_set(checkpoint_json,'$.version',3) WHERE request_id=?`, admitted.RequestID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MainUsage(context.Background(), v.ID); err == nil || !strings.Contains(err.Error(), "decode observed input checkpoint") {
		t.Fatal("corrupt durable checkpoint escaped validation")
	}
}

func TestMainUsageDoesNotAcquireHugeBodies(t *testing.T) {
	for _, version := range []int{0, 1, 2} {
		name := "missing checkpoint"
		if version == 1 {
			name = "legacy checkpoint"
		} else if version == 2 {
			name = "provider checkpoint"
		}
		t.Run(name, func(t *testing.T) {
			s, v, turn := usageFixture(t)
			// This visible payload predates admission, while the hidden audit has
			// no model-input role. Neither body is needed to read measured usage.
			huge := strings.Repeat("x", 12<<20)
			if _, err := s.Append(v.ID, turn, "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: huge}); err != nil {
				t.Fatal(err)
			}
			admitted := admitUsage(t, s, v, turn, version != 0)
			if _, err := s.Append(v.ID, turn, "main", "status", "", false, map[string]any{"type": "request_message", "message": map[string]string{"content": huge}}); err != nil {
				t.Fatal(err)
			}
			if err := s.FinishRequest(admitted.RequestID, RequestCompletion{Status: "completed", Attempts: []RequestAttempt{{Status: "completed", Usage: &llm.Usage{InputTokens: 100}, Error: huge}}}); err != nil {
				t.Fatal(err)
			}
			if version == 1 {
				if _, err := s.db.Exec(`UPDATE request_observations SET checkpoint_json=json_set(checkpoint_json,'$.version',1) WHERE request_id=?`, admitted.RequestID); err != nil {
					t.Fatal(err)
				}
			}
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			got := readUsage(t, s, v.ID)
			runtime.ReadMemStats(&after)
			if got.Reported == nil || got.Anchor == nil || got.Reported.Tokens.InputTokens != 100 || got.Anchor.RequestID != admitted.RequestID {
				t.Fatal(got)
			}
			if version == 0 {
				if got.Anchor.Checkpoint != nil {
					t.Fatal("fabricated checkpoint for legacy usage", got)
				}
			} else if got.Anchor.Checkpoint == nil || got.Anchor.Checkpoint.Version != version {
				t.Fatal("checkpoint changed while reading usage", got)
			}
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
				t.Fatalf("usage reader acquired large entry/attempt bodies: allocated %d bytes", allocated)
			}
		})
	}
}

func TestAdmissionCheckpointValidationAndAtomicity(t *testing.T) {
	s, v, turn := usageFixture(t)
	messages, err := s.Messages(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := usageCheckpoint(messages)
	before := sourceRecoveryState(t, s, v.ID)
	for _, mutate := range []func(*llm.TokenCheckpoint){
		func(c *llm.TokenCheckpoint) { c.Version = 3 },
		func(c *llm.TokenCheckpoint) { c.LocalInput = -1 },
		func(c *llm.TokenCheckpoint) { c.Compatible = "invalid" },
		func(c *llm.TokenCheckpoint) { c.Exact = strings.Repeat("z", 64) },
	} {
		bad := checkpoint
		mutate(&bad)
		if _, err := s.AdmitRequest(RequestAdmission{SessionID: v.ID, TurnID: turn, Actor: "main", Selection: v.Model, Checkpoint: &bad}); err == nil {
			t.Fatal("invalid checkpoint accepted", bad)
		}
	}
	if _, err := s.AdmitRequest(RequestAdmission{SessionID: v.ID, TurnID: turn, Actor: "main/child_usage", Selection: v.Model, Checkpoint: &checkpoint}); err == nil {
		t.Fatal("child accepted main checkpoint")
	}
	if after := sourceRecoveryState(t, s, v.ID); after != before {
		t.Fatal("invalid checkpoint admission changed durable state", after, before)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_checkpoint BEFORE UPDATE OF input_json ON model_requests
		WHEN json_extract(NEW.input_json,'$.input_estimate') IS NOT NULL
		BEGIN SELECT RAISE(ABORT,'injected checkpoint admission failure'); END`); err != nil {
		t.Fatal(err)
	}
	admitted, err := s.AdmitRequest(RequestAdmission{
		SessionID: v.ID, TurnID: turn, Actor: "main", Selection: v.Model, Checkpoint: &checkpoint,
		Context: &llm.Message{Type: llm.TypeRuntimeContext, Content: `{"type":"runtime_context"}`},
		Steers:  []llm.Message{{Type: llm.TypeUserInput, Content: "steered input"}},
	})
	if err == nil || !strings.Contains(err.Error(), "injected checkpoint admission failure") || admitted.RequestID != 0 || len(admitted.Messages) != 0 {
		t.Fatal("checkpoint failure returned committed admission", admitted, err)
	}
	if after := sourceRecoveryState(t, s, v.ID); after != before {
		t.Fatal("checkpoint failure committed partial admission", after, before)
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_checkpoint"); err != nil {
		t.Fatal(err)
	}
	admitted, err = s.AdmitRequest(RequestAdmission{SessionID: v.ID, TurnID: turn, Actor: "main", Selection: v.Model, Checkpoint: &checkpoint})
	if err != nil {
		t.Fatal(err)
	}
	var input string
	if err := s.db.QueryRow("SELECT input_json FROM model_requests WHERE id=?", admitted.RequestID).Scan(&input); err != nil {
		t.Fatal(err)
	}
	var metadata requestInputMetadata
	if err := json.Unmarshal([]byte(input), &metadata); err != nil || metadata.Version != 1 || metadata.MessageCount != len(admitted.Messages) || metadata.InputEstimate == nil || *metadata.InputEstimate != checkpoint {
		t.Fatal("admission did not atomically save checkpoint metadata", input, err)
	}
}
