package openai

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/history"
	"ttc/internal/llm"
)

// Exercise the real provider extraction and public history recording/projection
// APIs together: empty collection presence must survive item-level deltas.
func TestEmptyCollectionProviderHistoryDelta(t *testing.T) {
	for _, tc := range []struct {
		name, before, after, counts string
		wantEmptyJSON               bool
	}{
		{"absent-to-empty", `{ "model": "m" }`, "{\n \"model\":\"m\", \"input\":[], \"tools\":[]\n}\n", "2 added, 0 removed, 1 unchanged", true},
		{"empty-to-absent", `{ "model":"m", "input":[], "tools":[] }`, `{ "model": "m" }`, "0 added, 2 removed, 1 unchanged", false},
		{"nonempty-to-empty", `{"model":"m","input":[{"role":"user","content":"old"}],"tools":[{"type":"function","name":"old"}]}`, `{ "model":"m", "tools":[], "input":[] }`, "2 added, 2 removed, 1 unchanged", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := history.Open(filepath.Join(t.TempDir(), "data"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			selection := llm.Selection{Provider: "openai", Model: llm.ScriptModel()}
			session := history.NewID("session")
			turn, _, err := store.StartSession(session, t.TempDir(), selection, llm.Message{Type: llm.TypeUserInput, Content: "test"})
			if err != nil {
				t.Fatal(err)
			}
			record := func(raw string) int64 {
				t.Helper()
				doc, err := new(Adapter).InspectBoundary("request", json.RawMessage(raw))
				if err != nil {
					t.Fatal(err)
				}
				request, err := store.StartRequest(session, turn, "main", "coding", selection)
				if err != nil {
					t.Fatal(err)
				}
				items, err := store.RecordBoundary(session, turn, "main", request, "test", llm.BoundaryEvent{Direction: "request", Attempt: 1, Payload: json.RawMessage(raw), Inspection: &doc})
				if err != nil || len(items) != 1 {
					t.Fatal(items, err)
				}
				return items[0].ID
			}
			before, after := record(tc.before), record(tc.after)
			ctx := context.Background()
			compact, err := store.ReadInspection(ctx, after)
			if err != nil {
				t.Fatal(err)
			}
			expanded, err := store.BoundaryExpandedPage(ctx, after, 0, history.InspectionPageChars)
			if err != nil || expanded.Total > history.InspectionPageChars {
				t.Fatal(expanded, err)
			}
			for mode, text := range map[string]string{"compact": compact.Text, "expanded": expanded.Text} {
				if !strings.Contains(text, tc.counts) || strings.Contains(text, "No item changes.") {
					t.Fatalf("%s lost empty collection delta: %s", mode, text)
				}
				_, native, hasJSON := strings.Cut(text, "Prettified raw JSON")
				if tc.wantEmptyJSON {
					if !hasJSON || !strings.Contains(native, `"input": []`) || !strings.Contains(native, `"tools": []`) || strings.Contains(native, `"model"`) || strings.Contains(native, `"old"`) {
						t.Fatalf("%s native delta is not the full changed collection items: %s", mode, text)
					}
				} else if hasJSON {
					t.Fatalf("%s removal-only delta included native items: %s", mode, text)
				}
			}
			for id, want := range map[int64]string{before: tc.before, after: tc.after} {
				raw, err := store.BoundaryRaw(ctx, id)
				if err != nil || string(raw) != want {
					t.Fatalf("exact raw capture changed: %q want %q err=%v", raw, want, err)
				}
				page, err := store.BoundaryRawPage(ctx, id, 0, history.InspectionPageChars)
				if err != nil || page.Text != want {
					t.Fatalf("raw R page changed: %q want %q err=%v", page.Text, want, err)
				}
			}
		})
	}
}
