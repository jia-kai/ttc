package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/llm"
	"ttc/internal/render"
)

func TestTranscriptSubagentLaunchAndCompletion(t *testing.T) {
	s, v, turn, request := historyFixture(t)
	call := llm.ToolCall{ID: "launch", Name: "subagent", Arguments: []byte(`{"label":"helper","background":true}`)}
	id, err := s.CallIntent(v.ID, turn, "main", request, call)
	if err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(`{"assignment_status":"running","child_id":"main/child_test"}`)
	cut, err := s.CallResult(v.ID, turn, "main", id, result, nil, map[string]any{}, render.Tool(call.Name, call.Arguments, result), true)
	if err != nil {
		t.Fatal(err)
	}
	finish := `{"type":"child_turn_finished","child_id":"main/child_test","assignment_status":"failed","answer":"partial answer","error":"failure","warning":"warning"}`
	if _, err := s.Append(v.ID, turn, "main", "message", "user", true, llm.Message{Type: llm.TypeRuntimeNotification, Content: finish}); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []bool{false, true} {
		transcript := func(tip int64) []byte {
			t.Helper()
			var body []byte
			var err error
			if actor {
				body, err = s.ActorTranscript(v.ID, "main", tip)
			} else {
				body, err = s.Transcript(v.ID, tip)
			}
			if err != nil {
				t.Fatal(err)
			}
			return body
		}
		frozen := string(transcript(cut))
		if strings.Contains(frozen, "Assignment status: running") || strings.Contains(frozen, "partial answer") {
			t.Fatalf("launch result or later notification leaked into frozen cut: %s", frozen)
		}
		saved, err := s.Session(v.ID)
		if err != nil {
			t.Fatal(err)
		}
		body := string(transcript(saved.EntryTip))
		for _, want := range []string{`### main/child\_test · subagent response · #`, "Assignment status: failed", "partial answer", "Error: failure", "Warning: warning"} {
			if !strings.Contains(body, want) {
				t.Fatalf("missing %q: %s", want, body)
			}
		}
		if strings.Contains(body, "Assignment status: running") {
			t.Fatalf("incorrect transcript row: %s", body)
		}
	}
	exact, err := s.ActorTranscriptJSONL(v.ID, "main", cut)
	if err != nil || !bytes.Contains(exact, []byte(`"assignment_status":"running"`)) {
		t.Fatalf("exact export lost launch: %s (%v)", exact, err)
	}
}

func TestTranscriptBackgroundLaunchTerminalStatusWaitsForNotification(t *testing.T) {
	for _, status := range []string{"running", "completed", "failed"} {
		t.Run(status, func(t *testing.T) {
			s, v, turn, request := historyFixture(t)
			call := llm.ToolCall{ID: "launch", Name: "subagent", Arguments: []byte(`{"label":"helper","background":true}`)}
			id, err := s.CallIntent(v.ID, turn, "main", request, call)
			if err != nil {
				t.Fatal(err)
			}
			result := json.RawMessage(`{"assignment_status":"` + status + `","child_id":"main/child_test"}`)
			cut, err := s.CallResult(v.ID, turn, "main", id, result, nil, map[string]any{}, render.Tool(call.Name, call.Arguments, result), true)
			if err != nil {
				t.Fatal(err)
			}
			finish := `{"type":"child_turn_finished","child_id":"main/child_test","assignment_status":"` + status + `","answer":"actual answer"}`
			end, err := s.Append(v.ID, turn, "main", "message", "user", true, llm.Message{Type: llm.TypeRuntimeNotification, Content: finish})
			if err != nil {
				t.Fatal(err)
			}
			for _, actor := range []bool{false, true} {
				transcript := s.Transcript
				if actor {
					transcript = func(session string, tip int64) ([]byte, error) { return s.ActorTranscript(session, "main", tip) }
				}
				frozen, err := transcript(v.ID, cut)
				if err != nil || strings.Contains(string(frozen), "Assignment status:") || strings.Contains(string(frozen), "actual answer") || strings.Contains(string(frozen), "subagent response") {
					t.Fatalf("launch result leaked into frozen cut: %v\n%s", err, frozen)
				}
				body, err := transcript(v.ID, end)
				if err != nil || strings.Count(string(body), "Assignment status: "+status) != 1 || strings.Count(string(body), "actual answer") != 1 || !strings.Contains(string(body), `### main/child\_test · subagent response · #`) {
					t.Fatalf("notification projection: %v\n%s", err, body)
				}
			}
		})
	}
}

func TestInternalEventsRemainExactWithoutConversationDuplication(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	id, err := s.Append(v.ID, turn, "main", "status", "", false, map[string]any{"type": "job_state", "job_id": "job", "state": "running"})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.Entry(id)
	if err != nil {
		t.Fatal(err)
	}
	text, err := s.ExportText(entry)
	if err != nil || text != "" || !entry.InternalEvent() {
		t.Fatalf("bookkeeping became a conversation row: %q, %v", text, err)
	}
	inspection, err := s.ReadInspection(context.Background(), id)
	if err != nil || !strings.Contains(inspection.Text, "running") || inspection.Title != "job_state" {
		t.Fatalf("bookkeeping no longer inspectable: %+v, %v", inspection, err)
	}
	exact, err := s.TranscriptJSONL(v.ID, 0)
	if err != nil || !bytes.Contains(exact, []byte(`"type":"job_state"`)) {
		t.Fatal("exact transcript lost internal event", err)
	}
	for _, kind := range []string{"system_prompt", "job_completion", "child_turn_finished", "compaction_failed", "turn_end"} {
		entry.Content = json.RawMessage(`{"type":"` + kind + `"}`)
		if entry.InternalEvent() {
			t.Fatalf("user-facing event %s hidden", kind)
		}
	}
}

func TestChildMessageStatusExportsAndArchives(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	const message = "Check the results before continuing."
	id, err := s.Append(v.ID, turn, "main", "status", "", false, map[string]any{
		"type": "child_message", "child_id": "child", "child_turn_id": "assignment",
		"job_id": "job", "parent_call_id": "call", "message": message,
	})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.Entry(id)
	if err != nil {
		t.Fatal(err)
	}
	text, err := s.ExportText(entry)
	if err != nil || !strings.Contains(text, message) {
		t.Fatalf("export child-message status: %v\n%s", err, text)
	}
	path := filepath.Join(t.TempDir(), "export.md")
	if err := s.Export(v.ID, path); err != nil {
		t.Fatal(err)
	}
	archive, err := s.ArchiveActorTranscript(v.ID, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{path, archive} {
		markdown, err := os.ReadFile(path)
		if err != nil || !bytes.Contains(markdown, []byte(message)) {
			t.Fatalf("child message missing from %s: %v\n%s", path, err, markdown)
		}
		exact, err := os.ReadFile(path + ".jsonl")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, line := range bytes.Split(bytes.TrimSpace(exact), []byte{'\n'}) {
			var row struct{ Entry Entry }
			if err := json.Unmarshal(line, &row); err != nil {
				t.Fatal(err)
			}
			if row.Entry.ID != id {
				continue
			}
			if !bytes.Equal(row.Entry.Content, entry.Content) {
				t.Fatalf("child-message payload changed in %s: %s", path, row.Entry.Content)
			}
			found = true
		}
		if !found {
			t.Fatalf("child-message entry missing from %s.jsonl", path)
		}
	}
}

func TestGuidanceTranscriptDeduplicatesOnlySelectedSource(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	const actor = "main/child"
	const delivered = "Review the measurements."
	const pending = "Pending guidance stays visible."
	source, err := s.Append(v.ID, turn, "main", "status", "", false, map[string]any{
		"type": "child_message", "child_id": actor, "message": delivered,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"type": "child_message", "child_id": actor, "message": delivered, "event_seq": source})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := s.Append(v.ID, turn, actor, "message", "user", true, llm.Message{Type: llm.TypeChildGuidance, Content: string(payload), EventSeq: source})
	if err != nil {
		t.Fatal(err)
	}
	pendingID, err := s.Append(v.ID, turn, "main", "status", "", false, map[string]any{
		"type": "child_message", "child_id": actor, "message": pending,
	})
	if err != nil {
		t.Fatal(err)
	}
	check := func(label string, text []byte, expected string) {
		t.Helper()
		if strings.Count(string(text), expected) != 1 || strings.Contains(string(text), "```json") {
			t.Fatalf("%s should contain readable guidance exactly once:\n%s", label, text)
		}
	}
	for _, cut := range []int64{source, delivery, pendingID} {
		text, err := s.Transcript(v.ID, cut)
		if err != nil {
			t.Fatal(err)
		}
		check("selected cut", text, delivered)
		if cut == pendingID {
			check("pending", text, pending)
		}
	}
	path := filepath.Join(t.TempDir(), "all.md")
	if err := s.Export(v.ID, path); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	check("all-actor export", text, delivered)
	check("all-actor pending", text, pending)
	exact, err := os.ReadFile(path + ".jsonl")
	if err != nil || bytes.Count(exact, []byte(delivered)) != 2 {
		t.Fatalf("exact records lost: %v\n%s", err, exact)
	}
	for _, selected := range []string{"main", actor} {
		archive, err := s.ArchiveActorTranscript(v.ID, selected, nil)
		if err != nil {
			t.Fatal(err)
		}
		text, err := os.ReadFile(archive)
		if err != nil {
			t.Fatal(err)
		}
		check(selected, text, delivered)
	}
	// A child delivery selected without its source must not consult other rows.
	entry, err := s.Entry(delivery)
	if err != nil {
		t.Fatal(err)
	}
	text, err = s.transcriptEntries("child only", []Entry{entry})
	if err != nil {
		t.Fatal(err)
	}
	check("child-only selection", text, delivered)
}

func TestCompactionReplyInspectorUsesRequestPurpose(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	content := "## Handoff\n\n- Keep the current experiment.\n"
	for _, purpose := range []string{"compaction", "naming"} {
		request, err := s.StartRequest(v.ID, turn, "main", purpose, v.Model)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := s.RequestMessages(v.ID, turn, purpose, request, []llm.Message{{Type: llm.TypeAssistant, Content: content}})
		if err != nil {
			t.Fatal(err)
		}
		inspection, err := s.ReadInspection(context.Background(), ids[0])
		if err != nil {
			t.Fatal(err)
		}
		got := inspection.Text
		if purpose == "compaction" {
			if got != content {
				t.Fatalf("compaction reply escaped as metadata: %q", got)
			}
		} else if !strings.Contains(got, "```json") || !strings.Contains(got, `"type": "assistant"`) {
			t.Fatalf("naming inspector lost message metadata: %s", got)
		}
	}
}

func TestImageOnlyMessagesRemainInMarkdownAndArchive(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	path, err := s.Artifact(v.ID, "images", encoded.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(encoded.Bytes())
	file := llm.BinaryFile{Path: path, SHA256: hex.EncodeToString(sum[:]), MIMEType: "image/png", Bytes: encoded.Len()}
	message := llm.Message{Type: llm.TypeUserInput, Files: []llm.BinaryFile{file}}
	id, err := s.Append(v.ID, turn, "main", "message", "user", true, message)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.Entry(id)
	if err != nil {
		t.Fatal(err)
	}
	text, err := s.ExportText(entry)
	if err != nil || text != "Binary file: [snapshot](<"+path+">) · 2×3" {
		t.Fatal("image-only message lost or duplicated exact data", text, err)
	}
	archive, err := s.ArchiveActorTranscript(v.ID, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := os.ReadFile(archive)
	if err != nil || !strings.Contains(string(markdown), path) {
		t.Fatal("image absent from compaction Markdown", err)
	}
	exact, err := os.ReadFile(archive + ".jsonl")
	if err != nil || !bytes.Contains(exact, []byte(file.SHA256)) || bytes.Contains(exact, []byte("data_url")) || bytes.Contains(exact, []byte(base64.StdEncoding.EncodeToString(encoded.Bytes()))) {
		t.Fatal("sidecar lost reference or persisted inline image bytes", err)
	}
	var reloaded llm.Message
	if err := json.Unmarshal(entry.Content, &reloaded); err != nil || len(reloaded.Files) != 1 || reloaded.Files[0] != file {
		t.Fatal("history lost native image metadata", reloaded, err)
	}
}

func TestTranscriptJSONLFreezesSelectedCutAndExactPayloads(t *testing.T) {
	s, v, turn, request := historyFixture(t)
	prompt := "Stable <instructions>\nKeep exact whitespace.\n"
	if _, err := s.RecordSystemPrompt(v.ID, turn, "main", request, prompt); err != nil {
		t.Fatal(err)
	}
	native := json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque<private>"}`)
	call := llm.ToolCall{ID: "provider_call", Name: "read", Arguments: json.RawMessage(`{"path":"original<input>"}`)}
	_, calls, err := s.Assistant(v.ID, turn, "main", request, llm.Message{
		Type: llm.TypeAssistant, Content: "Inspect the file", Calls: []llm.ToolCall{call},
		State: &llm.ReplayState{Provider: "script", Model: v.Model.Model.ID, Version: 1, Items: []json.RawMessage{native}},
	})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.Session(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	cut := saved.EntryTip
	before, err := s.TranscriptJSONL(v.ID, cut)
	if err != nil {
		t.Fatal(err)
	}
	var foundPrompt, foundNative, foundCall bool
	for _, line := range bytes.Split(bytes.TrimSpace(before), []byte{'\n'}) {
		var row struct {
			Entry        Entry
			Instructions string
			Call         struct {
				ID, Name       string
				RequestID      int64  `json:"request_id"`
				ProviderCallID string `json:"provider_call_id"`
				Version        int
				Payload        json.RawMessage
			}
		}
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		if row.Instructions == prompt {
			foundPrompt = true
		}
		if row.Entry.Kind == "message" && row.Entry.Role == "assistant" {
			var message llm.Message
			if err := json.Unmarshal(row.Entry.Content, &message); err != nil {
				t.Fatal(err)
			}
			if message.State != nil && len(message.State.Items) == 1 {
				var replay struct {
					Type             string
					EncryptedContent string `json:"encrypted_content"`
				}
				if err := json.Unmarshal(message.State.Items[0], &replay); err != nil {
					t.Fatal(err)
				}
				foundNative = message.State.Provider == "script" && message.State.Model == v.Model.Model.ID && message.State.Version == 1 && replay.Type == "reasoning" && replay.EncryptedContent == "opaque<private>"
			}
		}
		if row.Entry.Kind == "tool_call" {
			foundCall = row.Call.ID == calls[0] && row.Call.Name == call.Name && row.Call.ProviderCallID == call.ID && row.Call.RequestID == request && row.Call.Version == 1 && bytes.Equal(row.Call.Payload, call.Arguments)
		}
	}
	if !foundPrompt || !foundNative || !foundCall {
		t.Fatalf("exact snapshot incomplete: prompt=%t native=%t call=%t\n%s", foundPrompt, foundNative, foundCall, before)
	}
	result := json.RawMessage(`{"ok":true,"content":"future-result<exact>"}`)
	delivered := json.RawMessage(`{"ok":false,"error":"model-delivery<notice>","reason":"batch_capacity"}`)
	originalFiles := []llm.BinaryFile{{Path: "immutable-fixture.png", MIMEType: "image/png", SHA256: strings.Repeat("a", 64), Bytes: 32}}
	exportBody := "Concise read result"
	md := render.Markdown{Revision: 1, Summary: "read", Detail: "expanded details", Export: &exportBody}
	resultID, err := s.CallResult(v.ID, turn, "main", calls[0], delivered, nil, map[string]any{"version": 1, "result": result, "files": originalFiles}, md, true)
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.TranscriptJSONL(v.ID, cut)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("future completion changed selected snapshot: %v\nbefore %s\nafter %s", err, before, after)
	}
	if bytes.Contains(after, []byte(`"delivered_result"`)) || bytes.Contains(after, []byte("model-delivery")) {
		t.Fatal("pending intent exposed delivery outside its frozen cut")
	}
	pending, err := s.Transcript(v.ID, cut)
	if err != nil || !strings.Contains(string(pending), "**read**") || strings.Contains(string(pending), prompt) || strings.Contains(string(pending), "future-result") {
		t.Fatalf("pending Markdown snapshot: %v\n%s", err, pending)
	}
	completed, err := s.Transcript(v.ID, resultID)
	if err != nil || strings.Count(string(completed), "**read**") != 1 || strings.Contains(string(completed), exportBody) || strings.Contains(string(completed), "expanded details") {
		t.Fatalf("completed Markdown projection: %v\n%s", err, completed)
	}
	exactCompleted, err := s.TranscriptJSONL(v.ID, resultID)
	if err != nil {
		t.Fatal(err)
	}
	var foundResult bool
	for _, line := range bytes.Split(bytes.TrimSpace(exactCompleted), []byte{'\n'}) {
		var row struct {
			Entry      Entry
			ToolRecord struct {
				Version int
				Files   []llm.BinaryFile
				Result  struct {
					OK      bool
					Content string
				}
			} `json:"tool_record"`
			Presentation    render.Markdown
			DeliveredResult json.RawMessage `json:"delivered_result"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		if row.Entry.ID == resultID {
			foundResult = row.ToolRecord.Version == 1 && row.ToolRecord.Result.OK && row.ToolRecord.Result.Content == "future-result<exact>" && row.Presentation.Detail == md.Detail && row.Presentation.Export != nil && *row.Presentation.Export == exportBody && bytes.Equal(row.DeliveredResult, delivered)
			if len(row.ToolRecord.Files) != 1 || row.ToolRecord.Files[0] != originalFiles[0] {
				t.Fatal("exact export lost original native file metadata", row.ToolRecord.Files)
			}
		} else if row.DeliveredResult != nil {
			t.Fatal("delivery attached to an entry other than its completed result", row.Entry.ID)
		}
	}
	if !foundResult {
		t.Fatalf("completed exact records missing:\n%s", exactCompleted)
	}
	actorPending, err := s.ActorTranscriptJSONL(v.ID, "main", cut)
	if err != nil || bytes.Contains(actorPending, []byte("model-delivery")) || bytes.Contains(actorPending, []byte("future-result")) {
		t.Fatal("actor snapshot leaked future completion", err, string(actorPending))
	}
	actorCompleted, err := s.ActorTranscriptJSONL(v.ID, "main", resultID)
	if err != nil || !bytes.Equal(actorCompleted, exactCompleted) {
		t.Fatal("actor exact export lost execution or delivery", err, string(actorCompleted))
	}
	archive, err := s.writeArchive(v.ID, pending, before)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		path string
		want []byte
	}{{archive, pending}, {archive + ".jsonl", before}} {
		got, err := os.ReadFile(file.path)
		if err != nil || !bytes.Equal(got, file.want) {
			t.Fatalf("archive cut differs from export projection: %s: %v", file.path, err)
		}
	}
	duplicate, err := s.writeArchive(v.ID, pending, before)
	if err != nil || duplicate != archive {
		t.Fatalf("immutable archive not deduplicated: %q %v", duplicate, err)
	}
}
