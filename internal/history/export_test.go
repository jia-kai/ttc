package history

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"strings"
	"testing"

	"ttc/internal/provider"
	"ttc/internal/render"
)

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
	detail, err := s.Inspect(entry)
	if err != nil || !strings.Contains(detail, "running") || s.Label(entry) != "job_state" {
		t.Fatalf("bookkeeping no longer inspectable: %q, %v", detail, err)
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

func TestCompactionReplyInspectorUsesRequestPurpose(t *testing.T) {
	s, v, turn, request := historyFixture(t)
	content := "## Handoff\n\n- Keep the current experiment.\n"
	for _, purpose := range []string{"compaction", "naming"} {
		id, err := s.RequestMessage(v.ID, turn, purpose, "assistant", request, provider.Message{Role: "assistant", Content: content})
		if err != nil {
			t.Fatal(err)
		}
		entry, err := s.Entry(id)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Inspect(entry)
		if err != nil {
			t.Fatal(err)
		}
		if purpose == "compaction" {
			if got != content {
				t.Fatalf("compaction reply escaped as metadata: %q", got)
			}
		} else if !strings.Contains(got, "```json") || !strings.Contains(got, `"role": "assistant"`) {
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
	message := provider.Message{Role: "user", Images: []provider.Image{{Path: path, DataURL: "data:image/png;base64,exact-image-payload"}}}
	id, err := s.Append(v.ID, turn, "main", "message", "user", true, message)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.Entry(id)
	if err != nil {
		t.Fatal(err)
	}
	text, err := s.ExportText(entry)
	if err != nil || !strings.Contains(text, "2×3") || !strings.Contains(text, path) || strings.Contains(text, "data:image") {
		t.Fatal("image-only message lost or duplicated exact data", text, err)
	}
	archive, err := s.ArchiveTranscript(v.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := os.ReadFile(archive)
	if err != nil || !strings.Contains(string(markdown), path) {
		t.Fatal("image absent from compaction Markdown", err)
	}
	exact, err := os.ReadFile(archive + ".jsonl")
	if err != nil || !strings.Contains(string(exact), message.Images[0].DataURL) {
		t.Fatal("exact image absent from sidecar", err)
	}
}

func TestTranscriptJSONLFreezesSelectedCutAndExactPayloads(t *testing.T) {
	s, v, turn, request := historyFixture(t)
	prompt := "Stable <instructions>\nKeep exact whitespace.\n"
	if _, err := s.RecordSystemPrompt(v.ID, turn, "main", request, prompt); err != nil {
		t.Fatal(err)
	}
	native := json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque<private>"}`)
	call := provider.ToolCall{ID: "provider_call", Name: "read", Arguments: json.RawMessage(`{"path":"original<input>"}`)}
	_, calls, err := s.Assistant(v.ID, turn, "main", request, provider.Message{
		Role: "assistant", Content: "Inspect the file", Calls: []provider.ToolCall{call},
		State: &provider.ReplayState{Provider: "script", Model: v.Model.Model.ID, Version: 1, Items: []json.RawMessage{native}},
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
			var message provider.Message
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
	exportBody := "Concise read result"
	md := render.Markdown{Revision: 1, Summary: "read", Detail: "expanded details", Export: &exportBody}
	resultID, err := s.CallResult(v.ID, turn, "main", calls[0], result, map[string]any{"version": 1, "result": result}, md, true)
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.TranscriptJSONL(v.ID, cut)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("future completion changed selected snapshot: %v\nbefore %s\nafter %s", err, before, after)
	}
	pending, err := s.Transcript(v.ID, cut)
	if err != nil || !strings.Contains(string(pending), "Pending call:") || strings.Contains(string(pending), prompt) || strings.Contains(string(pending), "future-result") {
		t.Fatalf("pending Markdown snapshot: %v\n%s", err, pending)
	}
	completed, err := s.Transcript(v.ID, resultID)
	if err != nil || !strings.Contains(string(completed), exportBody) || strings.Contains(string(completed), "Pending call:") || strings.Contains(string(completed), "expanded details") {
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
				Result  struct {
					OK      bool
					Content string
				}
			} `json:"tool_record"`
			Presentation render.Markdown
		}
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		if row.Entry.ID == resultID {
			foundResult = row.ToolRecord.Version == 1 && row.ToolRecord.Result.OK && row.ToolRecord.Result.Content == "future-result<exact>" && row.Presentation.Detail == md.Detail && row.Presentation.Export != nil && *row.Presentation.Export == exportBody
		}
	}
	if !foundResult {
		t.Fatalf("completed exact records missing:\n%s", exactCompleted)
	}
	archive, err := s.ArchiveTranscript(v.ID, cut)
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
	duplicate, err := s.ArchiveTranscript(v.ID, cut)
	if err != nil || duplicate != archive {
		t.Fatalf("immutable archive not deduplicated: %q %v", duplicate, err)
	}
}
