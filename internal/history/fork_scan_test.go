package history

import (
	"encoding/json"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode"

	"ttc/internal/llm"
)

func TestForkJSONNameFoldAliases(t *testing.T) {
	aliases := map[rune]rune{}
	for ascii := 'a'; ascii <= 'z'; ascii++ {
		for folded := unicode.SimpleFold(ascii); folded != ascii; folded = unicode.SimpleFold(folded) {
			if folded > unicode.MaxASCII {
				aliases[folded] = ascii
			}
		}
	}
	if want := map[rune]rune{'ſ': 's', 'K': 'k'}; !reflect.DeepEqual(aliases, want) {
		t.Fatalf("Go's ASCII JSON name folding changed; update bounded SQL normalization: got %v want %v", aliases, want)
	}
}

func TestForkCanonicalProjectionMatchesAuthoritativeDecoder(t *testing.T) {
	s, _, _, _ := historyFixture(t)
	for _, raw := range []string{
		`{"type":"assistant","callſ":[{"id":"open","name":"read","argumentſ":{"large":"opaque"}}]}`,
		`{"type":"assistant","calls":[{"id":"old"}],"callſ":[{"id":"last"}]}`,
		`{"type":"assistant","calls":[{"id":"old"}],"callſ":null}`,
		`{"type":"assistant","callſ":{}}`,
		`{"type":"user_input","ſtate":{"provider":"p","items":[]}}`,
		`{"type":"user_input","STATE":{"provider":"p"},"ſtate":null}`,
		`{"type":"assistant","ſtate":{"provider":"p","itemſ":123}}`,
		`{"type":"assistant","state":{"provider":"p"},"ſtate":{"model":"m","itemſ":[]}}`,
		`{"type":"assistant","ſtate":true}`,
		`{"type":"user_input","input_ſource":"steer","input_time_mſ":42,"retained_at_mſ":50,"uſer_text":"instruction"}`,
		`{"type":"assistant","retained_at_mſ":50}`,
		`{"type":"assistant","fileſ":[{"path":"x","byteſ":"bad"}]}`,
		`{"type":"assistant","fileſ":[{"path":"x","byteſ":12}]}`,
		`{"type":"assistant","CONTENT":123}`,
		`{"type":"assistant","TYPE":null,"event_ſeq":17,"requeſt_id":19,"phaſe":"final"}`,
		`{"type":"assistant","runtime":false}`,
		`{"type":"assistant","Runtime":false}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var original, projected llm.Message
			originalErr := json.Unmarshal([]byte(raw), &original)
			var compact string
			if err := s.db.QueryRow("SELECT "+forkCanonicalJSON+" FROM (SELECT ? AS content_json) e", raw).Scan(&compact); err != nil {
				t.Fatal(err)
			}
			projectedErr := json.Unmarshal([]byte(compact), &projected)
			if (originalErr == nil) != (projectedErr == nil) {
				t.Fatalf("decoder/projection disagreement: canonical=%v projected=%v envelope=%s", originalErr, projectedErr, compact)
			}
			if originalErr != nil {
				return
			}
			if original.Type != projected.Type || original.EventSeq != projected.EventSeq || original.RequestID != projected.RequestID || original.InputSource != projected.InputSource || original.InputTimeMS != projected.InputTimeMS || original.RetainedAtMS != projected.RetainedAtMS || original.Phase != projected.Phase || original.CallID != projected.CallID || (original.UserText == nil) != (projected.UserText == nil) || !reflect.DeepEqual(original.Files, projected.Files) || (original.State == nil) != (projected.State == nil) || len(original.Calls) != len(projected.Calls) {
				t.Fatalf("projection changed semantic metadata: original=%+v projected=%+v", original, projected)
			}
			for i, call := range original.Calls {
				if call.ID != projected.Calls[i].ID || call.Name != projected.Calls[i].Name {
					t.Fatal("projection changed call identity", original.Calls, projected.Calls)
				}
			}
			if original.State != nil && (original.State.Provider != projected.State.Provider || original.State.Model != projected.State.Model || original.State.Version != projected.State.Version) {
				t.Fatal("projection changed replay metadata", original.State, projected.State)
			}
		})
	}
}

func TestForkBalancesUnicodeFoldedCallField(t *testing.T) {
	s, source, turn, _ := historyFixture(t)
	before, err := s.Session(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"type":"assistant","callſ":[{"id":"open-call","name":"read","argumentſ":{}}]}`)
	id, err := s.Append(source.ID, turn, "main", "message", "assistant", true, raw)
	if err != nil {
		t.Fatal(err)
	}
	fork, err := s.Load(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fork.EntryTip != before.EntryTip || fork.EntryTip == id {
		t.Fatal("fork accepted an unresolved Unicode-folded call batch", fork.EntryTip, before.EntryTip, id)
	}
	messages, err := s.Messages(fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if len(message.Calls) != 0 {
			t.Fatal("unbalanced call leaked into fork input", message.Calls)
		}
	}
}

func TestForkVisibleReadsClassifyRuntimeBeforeBodyAcquisition(t *testing.T) {
	s, source, turn, _ := historyFixture(t)
	large := strings.Repeat("runtime noise ", 1<<20)
	runtimeID, err := s.Append(source.ID, turn, "main", "message", "developer", true, llm.Message{Type: llm.TypeRuntimeContext, Content: large})
	if err != nil {
		t.Fatal(err)
	}
	// Preserve noncanonical whitespace and an unknown extension as exact source
	// evidence. A surviving human message must not expose its scan projection.
	const original = ` { "type":"user_input", "content":"hello", "extension":9007199254740993 } `
	if _, err := s.db.Exec("UPDATE entries SET content_json=? WHERE id=?", original, source.EntryTip); err != nil {
		t.Fatal(err)
	}
	fork, err := s.Load(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Messages(fork.ID); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	messages, err := s.Messages(fork.ID)
	runtime.ReadMemStats(&after)
	if err != nil || len(messages) != 1 || messages[0].Content != "hello" {
		t.Fatal("runtime masking changed model input", len(messages), err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > uint64(len(large)/2) {
		t.Fatal("visible-only read acquired inherited runtime body", allocated, len(large))
	}
	visible, err := branchWith(s.db, fork.ID, 0, true)
	if err != nil || len(visible) != 1 || string(visible[0].Content) != original {
		t.Fatal("surviving message did not retain exact canonical bytes", err)
	}
	all, err := s.Branch(fork.ID, 0)
	if err != nil || len(all) != 3 || all[len(all)-1].ID != runtimeID || !strings.Contains(string(all[len(all)-1].Content), large) {
		t.Fatal("full history lost inherited runtime evidence", err)
	}
}
