package history

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"ttc/internal/llm"
)

func TestInspectionModelTextPages(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	text := "界🙂e\u0301\n\t<raw>\x1b[31m\r\n" + strings.Repeat("λ中🙂", 80)
	guidance, err := json.Marshal(map[string]any{
		"type": "child_message", "message": text, "job_read_grants": []string{"job_界", "job_🙂"},
	})
	if err != nil {
		t.Fatal(err)
	}
	contextJSON, err := json.Marshal(map[string]any{"type": "runtime_context", "cwd": text, "jobs": []any{map[string]any{"label": "界🙂"}}})
	if err != nil {
		t.Fatal(err)
	}
	messages := []llm.Message{
		{Type: llm.TypeRuntimeContext, Content: string(contextJSON)},
		{Type: llm.TypeRuntimeContext, Content: `{"type":"runtime_context_update","set":{"branch":"界🙂","jobs":[]}}`},
		{Type: llm.TypeRuntimeNotification, Content: `{"type":"job_exit","message":"界🙂\\n","event_seq":9,"message_type":"ignored"}`},
		{Type: llm.TypeRuntimeNotification, Content: text},
		{Type: llm.TypeRuntimeInstruction, Content: text},
		{Type: llm.TypeCompactionContext, Content: text},
		{Type: llm.TypeChildGuidance, Content: string(guidance)},
		{Type: llm.TypeUserInput, InputSource: "steer", Content: text},
		{Type: llm.TypeUserInput, InputSource: "steer"},
	}
	for _, kind := range []string{"message", "summary", "request_message"} {
		for i, message := range messages {
			t.Run(kind+"/"+string(message.Type)+"/"+string(rune('a'+i)), func(t *testing.T) {
				want, err := message.ModelContent()
				if err != nil {
					t.Fatal(err)
				}
				entryKind, role, visible := kind, message.Role(), true
				var payload any = message
				if kind == "request_message" {
					entryKind, role, visible = "status", "", false
					payload = map[string]any{"type": kind, "purpose": "coding", "role": message.Role(), "message": message}
				}
				id, err := s.Append(session.ID, turn, "main", entryKind, role, visible, payload)
				if err != nil {
					t.Fatal(err)
				}
				var joined strings.Builder
				const limit = 17 // Deliberately split JSON/YAML, tags and multi-byte text.
				total := utf8.RuneCountInString(want)
				for offset := 0; offset < total; offset += limit {
					page, err := s.InspectPage(context.Background(), id, offset, limit)
					if err != nil {
						t.Fatal(err)
					}
					if !page.Supported || page.Markdown || page.JSON || page.System != contextPresentation(message.Type) || page.Total != total || page.Offset != offset || page.Limit != limit || !utf8.ValidString(page.Text) || utf8.RuneCountInString(page.Text) > limit {
						t.Fatalf("invalid projected page: %+v", page)
					}
					joined.WriteString(page.Text)
				}
				if joined.String() != want {
					t.Fatalf("reassembled model text differs:\ngot  %q\nwant %q", joined.String(), want)
				}
				for _, offset := range []int{total, total + 5} {
					page, err := s.InspectPage(context.Background(), id, offset, limit)
					if err != nil || !page.Supported || page.Total != total || page.Text != "" || page.Markdown || page.JSON {
						t.Fatalf("page past end: %+v, %v", page, err)
					}
				}
			})
		}
	}
}

func TestInspectionModelTextInvalidProjection(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	for _, message := range []llm.Message{
		{Type: llm.TypeRuntimeContext, Content: `{"type":"runtime_context",`},
		{Type: llm.TypeRuntimeContext, Content: `{"type":"unknown"}`},
		{Type: llm.TypeRuntimeContext, Content: `{"type":"runtime_context_update","set":[]}`},
		{Type: llm.TypeChildGuidance, Content: "not JSON"},
		{Type: llm.TypeChildGuidance, Content: `{"type":"child_message"}`},
	} {
		for _, nested := range []bool{false, true} {
			kind, role := "message", message.Role()
			var payload any = message
			if nested {
				kind, role = "status", ""
				payload = map[string]any{"type": "request_message", "purpose": "compaction", "role": message.Role(), "message": message}
			}
			id, err := s.Append(session.ID, turn, "main", kind, role, false, payload)
			if err != nil {
				t.Fatal(err)
			}
			// Even an empty/out-of-range page must validate complete content.
			if _, err := s.InspectPage(context.Background(), id, 9999, 1); err == nil {
				t.Fatalf("invalid projection accepted (nested=%v): %+v", nested, message)
			}
		}
	}
}

func TestInspectionModelTextLargeEnvelopesStayInSQLite(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	// Large file metadata and replay items must not be decoded/transferred just
	// to inspect a short body. Runtime messages do not have native replay state.
	large := strings.Repeat("x", 8<<20)
	messages := []llm.Message{
		{Type: llm.TypeUserInput, InputSource: "steer", Content: "界🙂", Files: []llm.BinaryFile{{Path: large, MIMEType: "image/png", Bytes: 1}}},
		{Type: llm.TypeRuntimeNotification, Content: `{"type":"job_exit","label":"界🙂"}`, Files: []llm.BinaryFile{{Path: large, MIMEType: "image/png", Bytes: 1}}},
		{Type: llm.TypeAssistant, Content: "Done.", State: &llm.ReplayState{Provider: "fixture", Model: "fixture", Version: 1, Items: []json.RawMessage{json.RawMessage(`{"opaque":"` + large + `"}`)}}},
	}
	for _, nested := range []bool{false, true} {
		for _, message := range messages {
			kind, role := "message", message.Role()
			var payload any = message
			if nested {
				kind, role = "status", ""
				payload = map[string]any{"type": "request_message", "purpose": "compaction", "role": message.Role(), "message": message}
			}
			id, err := s.Append(session.ID, turn, "main", kind, role, !nested, payload)
			if err != nil {
				t.Fatal(err)
			}
			// A full JSON envelope transferred to Go alone would exceed this
			// allocation budget, independent of whether it is then decoded.
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			page, err := s.InspectPage(context.Background(), id, 0, InspectionPageChars)
			runtime.ReadMemStats(&after)
			if err != nil {
				t.Fatal(err)
			}
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 2<<20 {
				t.Fatalf("large envelope transferred to Go (nested=%v, type=%s): allocated %d bytes", nested, message.Type, allocated)
			}
			if !page.Supported || !page.LargeEnvelope || len(page.Text) > 200 || page.Total > 200 {
				t.Fatalf("unbounded inspection (nested=%v): %+v", nested, page)
			}
			want, err := message.ModelContent()
			if err != nil || page.Text != want {
				t.Fatalf("large envelope changed body (nested=%v): %q, want %q, %v", nested, page.Text, want, err)
			}
		}
	}
}

func TestInspectionModelTextLargeBodyBoundedPages(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	content := strings.Repeat("界🙂λ", 10000)
	encoded, err := json.Marshal(map[string]any{"type": "runtime_context", "text": content})
	if err != nil {
		t.Fatal(err)
	}
	message := llm.Message{Type: llm.TypeRuntimeContext, Content: string(encoded)}
	want, err := message.ModelContent()
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Append(session.ID, turn, "main", "message", message.Role(), true, message)
	if err != nil {
		t.Fatal(err)
	}
	var joined strings.Builder
	for offset := 0; offset < utf8.RuneCountInString(want); offset += InspectionPageChars {
		page, err := s.InspectPage(context.Background(), id, offset, InspectionPageChars)
		if err != nil || !page.Supported || page.Markdown || page.JSON || page.Total != utf8.RuneCountInString(want) || utf8.RuneCountInString(page.Text) > InspectionPageChars || len(page.Text) > 64<<10 {
			t.Fatalf("large model-text page: %+v, %v", page, err)
		}
		joined.WriteString(page.Text)
	}
	if joined.String() != want {
		t.Fatal("large structured body did not reassemble exactly")
	}
}

func TestInspectionModelTextLeavesOrdinaryMessagesUnchanged(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	for _, message := range []llm.Message{
		{Type: llm.TypeUserInput, Content: `{"text":"界🙂"}`},
		{Type: llm.TypeUserInput, InputSource: "queue", Content: "<steer>authored</steer>"},
		{Type: llm.TypeAssistant, Content: "# Markdown"},
	} {
		id, err := s.Append(session.ID, turn, "main", "message", message.Role(), true, message)
		if err != nil {
			t.Fatal(err)
		}
		page, err := s.InspectPage(context.Background(), id, 0, InspectionPageChars)
		if err != nil || !page.Supported || !page.Markdown || page.Text != message.Content || page.JSON != json.Valid([]byte(message.Content)) {
			t.Fatalf("ordinary message changed: %+v, %v", page, err)
		}
	}
}

func TestInspectionModelTextAcceptedRawScalars(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	for _, test := range []struct{ name, raw string }{
		// The independent review's six original failures are covered by these
		// direct fixtures and their nested request_message variants below.
		{"uppercase content", `{"type":"runtime_notification","Content":"actual notification"}`},
		{"duplicate content", `{"type":"runtime_notification","content":"stale","content":"actual notification"}`},
		{"duplicate type", `{"type":"user_input","type":"runtime_notification","content":"actual notification"}`},
		{"uppercase input source", `{"type":"user_input","INPUT_SOURCE":"steer","content":"actual guidance"}`},
		{"uppercase type", `{"TYPE":"runtime_notification","content":"界🙂"}`},
		{"mixed duplicate type", `{"TYPE":"user_input","Type":"runtime_notification","content":"界🙂"}`},
		{"null final type", `{"type":"runtime_notification","TYPE":null,"content":"界🙂"}`},
		{"null initial type", `{"Type":null,"type":"runtime_notification","content":"界🙂"}`},
		{"mixed duplicate content", `{"type":"runtime_notification","Content":"stale","CONTENT":"界🙂"}`},
		{"null final content", `{"type":"runtime_notification","content":"界🙂","Content":null}`},
		{"null initial content", `{"type":"runtime_notification","CONTENT":null,"content":"界🙂"}`},
		{"empty final content then null", `{"type":"runtime_notification","content":"stale","CONTENT":"","Content":null}`},
		{"only null content", `{"type":"runtime_notification","Content":null,"CONTENT":null}`},
		{"mixed duplicate input source", `{"type":"user_input","input_source":"queue","INPUT_SOURCE":"steer","content":"界🙂"}`},
		{"null final input source", `{"type":"user_input","input_source":"steer","Input_Source":null,"content":"界🙂"}`},
		{"null initial input source", `{"type":"user_input","INPUT_SOURCE":null,"input_source":"steer","content":"界🙂"}`},
		{"escaped keys", `{"\u0074ype":"user_input","\u0069nput_source":"steer","\u0043ontent":"界🙂"}`},
		{"unicode case fold", `{"type":"user_input","input_ſource":"steer","content":"界🙂"}`},
	} {
		for _, kind := range []string{"message", "summary", "status"} {
			t.Run(kind+"/"+test.name, func(t *testing.T) {
				// Put ignored data first to exercise high json_each IDs and force
				// the same LargeEnvelope routing used by the terminal inspector.
				raw := json.RawMessage(`{"ignored":"` + strings.Repeat("x", 70000) + `",` + strings.TrimPrefix(test.raw, "{"))
				var message llm.Message
				if err := json.Unmarshal(raw, &message); err != nil {
					t.Fatal(err)
				}
				role, visible := message.Role(), true
				if kind == "status" {
					raw = json.RawMessage(`{"type":"request_message","purpose":"coding","role":"` + role + `","message":` + string(raw) + `}`)
					role, visible = "", false
				}
				want, err := message.ModelContent()
				if err != nil {
					t.Fatal(err)
				}
				id, err := s.Append(session.ID, turn, "main", kind, role, visible, raw)
				if err != nil {
					t.Fatal(err)
				}
				small, err := s.ReadInspection(context.Background(), id)
				if err != nil || small.Text != want {
					t.Fatalf("small inspector: %q, want %q: %v", small.Text, want, err)
				}
				page, err := s.InspectPage(context.Background(), id, 0, InspectionPageChars)
				if err != nil || !page.Supported || !page.LargeEnvelope || page.Text != want || page.Markdown || page.JSON || page.Total != utf8.RuneCountInString(want) || page.System != small.System {
					t.Fatalf("accepted raw fields differ: %+v, want %q: %v", page, want, err)
				}
			})
		}
	}
}

func TestInspectionModelTextAcceptedNestedEnvelopes(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	for _, test := range []struct{ name, raw string }{
		{"casefold envelope", `{"TYPE":"request_message","PURPOSE":"coding","ROLE":"user","MESSAGE":{"type":"runtime_notification","Content":"界🙂"}}`},
		{"unicode message key", `{"type":"request_message","purpose":"coding","role":"user","meſſage":{"type":"runtime_notification","content":"界🙂"}}`},
		{"duplicate discriminator", `{"type":"job_completion","TYPE":"request_message","purpose":"coding","role":"user","message":{"type":"runtime_notification","content":"界🙂"}}`},
		{"null final discriminator", `{"Type":"request_message","type":null,"purpose":"coding","role":"user","message":{"type":"runtime_notification","content":"界🙂"}}`},
		{"duplicate message", `{"type":"request_message","purpose":"coding","role":"user","message":{"type":"runtime_notification","content":"stale"},"MESSAGE":{"type":"runtime_notification","content":"界🙂"}}`},
		// llm.Message's custom unmarshaler REPLACES each object; it does
		// not merge fields as an ordinary nested struct decoder would.
		{"duplicate message resets content", `{"type":"request_message","purpose":"coding","role":"user","message":{"type":"runtime_notification","content":"stale"},"Message":{"type":"runtime_notification"}}`},
		{"duplicate message resets source", `{"type":"request_message","purpose":"coding","role":"user","message":{"type":"user_input","input_source":"steer","content":"stale"},"message":{"type":"runtime_notification","content":"界🙂"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := json.RawMessage(`{"ignored":"` + strings.Repeat("x", 70000) + `",` + strings.TrimPrefix(test.raw, "{"))
			var request struct{ Message llm.Message }
			if err := json.Unmarshal(raw, &request); err != nil {
				t.Fatal(err)
			}
			if test.name == "duplicate message resets content" && request.Message.Content != "" {
				t.Fatal("nested decoder unexpectedly merged Content")
			}
			if request.Message.InputSource != "" {
				t.Fatal("nested decoder unexpectedly merged InputSource")
			}
			want, err := request.Message.ModelContent()
			if err != nil {
				t.Fatal(err)
			}
			id, err := s.Append(session.ID, turn, "main", "status", "", false, raw)
			if err != nil {
				t.Fatal(err)
			}
			// ReadInspection exercises the actual decodeInspectionBody path,
			// not a stand-in struct that would merge duplicate message fields.
			small, err := s.ReadInspection(context.Background(), id)
			if err != nil || small.Text != want {
				t.Fatalf("small inspector: %q, want %q: %v", small.Text, want, err)
			}
			page, err := s.InspectPage(context.Background(), id, 0, InspectionPageChars)
			if err != nil || !page.Supported || !page.LargeEnvelope || page.Type != "request_message" || page.Text != want || page.Markdown || page.JSON {
				t.Fatalf("nested accepted envelope differs: %+v, want %q: %v", page, want, err)
			}
		})
	}
}

func TestInspectionModelTextLastScalarControlsSelection(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"type":"runtime_notification","TYPE":"user_input","content":"ordinary"}`),
		json.RawMessage(`{"type":"runtime_context","TYPE":"user_input","content":"ordinary"}`),
		json.RawMessage(`{"type":"user_input","input_source":"steer","INPUT_SOURCE":"queue","content":"ordinary"}`),
		json.RawMessage(`{"type":"user_input","input_source":"steer","INPUT_SOURCE":"","Input_Source":null,"content":"ordinary"}`),
	} {
		var message llm.Message
		if err := json.Unmarshal(raw, &message); err != nil {
			t.Fatal(err)
		}
		id, err := s.Append(session.ID, turn, "main", "message", message.Role(), true, raw)
		if err != nil {
			t.Fatal(err)
		}
		page, err := s.InspectPage(context.Background(), id, 0, InspectionPageChars)
		if err != nil || !page.Supported || !page.Markdown || page.Text != "ordinary" || page.Type != string(message.Type) || page.System {
			t.Fatalf("stale discriminator selected projection: %+v, %v", page, err)
		}
	}
}

func TestInspectionModelTextRawLargeEnvelopeStaysInSQLite(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	large := strings.Repeat("x", 8<<20)
	for _, nested := range []bool{false, true} {
		// Even stale duplicate content must not be transferred when a short
		// final value is authoritative. The initial unknown field is omitted.
		raw := json.RawMessage(`{"ignored":"` + large + `","TYPE":"runtime_notification","content":"` + large + `","CONTENT":"界🙂","Content":null}`)
		kind, role, visible := "message", "user", true
		if nested {
			raw = json.RawMessage(`{"type":"other","TYPE":"request_message","message":{"type":"runtime_notification","content":"stale"},"MESSAGE":` + string(raw) + `}`)
			kind, role, visible = "status", "", false
		}
		id, err := s.Append(session.ID, turn, "main", kind, role, visible, raw)
		if err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		page, err := s.InspectPage(context.Background(), id, 0, InspectionPageChars)
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatal(err)
		}
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 2<<20 {
			t.Fatalf("raw envelope transferred to Go (nested=%v): allocated %d bytes", nested, allocated)
		}
		want, err := (llm.Message{Type: llm.TypeRuntimeNotification, Content: "界🙂"}).ModelContent()
		if err != nil || !page.Supported || !page.LargeEnvelope || page.Text != want || page.Markdown || page.JSON {
			t.Fatalf("raw large envelope changed projection (nested=%v): %+v, want %q, %v", nested, page, want, err)
		}
	}
}
