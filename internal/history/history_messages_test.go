package history

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unicode"

	"ttc/internal/llm"
	"ttc/internal/prompts"
)

func requireHistoryMessage(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || err.Error() != want {
		t.Fatalf("history diagnostic = %v, want %q", err, want)
	}
}

func TestHistoryChildCompletionMessages(t *testing.T) {
	valid := ChildFinish{ChildID: "main/child", TurnID: "child-turn", JobID: "job", Status: "completed"}
	cases := []struct {
		name string
		edit func(*ChildFinish)
		want string
	}{
		{"IDs", func(f *ChildFinish) { f.JobID = "" }, "child completion requires child and turn IDs; successful assignments require a job ID"},
		{"status", func(f *ChildFinish) { f.Status = "unknown" }, "invalid child completion status"},
		{"result", func(f *ChildFinish) { f.ResultEntry = -1 }, "child completion result entry must be nonnegative"},
		{"UTF-8", func(f *ChildFinish) { f.Answer = "\xff" }, "child answer must be valid UTF-8 and at most 8 KiB"},
		{"size", func(f *ChildFinish) { f.Answer = strings.Repeat("x", MaxChildAnswerBytes+1) }, "child answer must be valid UTF-8 and at most 8 KiB"},
		{"paths", func(f *ChildFinish) { f.TranscriptPath = "archive.md" }, "child transcript requires both artifact paths or an export error, never both"},
		{"partial", func(f *ChildFinish) { f.Status, f.Answer = "failed", "partial" }, "partial child answers require a warning and transcript paths or an explicit export error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			finish := valid
			tc.edit(&finish)
			requireHistoryMessage(t, validateChildFinish(finish), tc.want)
		})
	}
	s, v, _, _ := historyFixture(t)
	_, err := s.FinishChildTurn(v.ID, valid)
	requireHistoryMessage(t, err, "child turn child-turn is missing or already terminal")
}

func TestHistoryPersistenceMessages(t *testing.T) {
	s, v, turn, request := historyFixture(t)
	_, err := s.CallIntent(v.ID, turn, "main/child", request, llm.ToolCall{Arguments: json.RawMessage("invalid")})
	requireHistoryMessage(t, err, "tool arguments are not JSON")
	// Assistant marshaling rejects invalid raw arguments before its transaction
	// invariant can run; keep that invariant's diagnostic bytes pinned too.
	if prompts.HistoryAssistantArguments != "invalid provider call arguments" {
		t.Fatal("assistant validation diagnostic changed", prompts.HistoryAssistantArguments)
	}
	err = s.transact(func(tx *sql.Tx) error {
		_, err := appendTx(tx, v.ID, turn, "main/child", "message", "assistant", false, json.RawMessage("invalid"), 0)
		return err
	})
	requireHistoryMessage(t, err, "invalid history JSON")
	_, err = s.RecordSystemPrompt(v.ID, turn, "main/child", request, strings.Repeat("x", instructionSnapshotBytes+1))
	requireHistoryMessage(t, err, "instruction snapshot exceeds 1 MiB")
	_, err = s.Artifact(v.ID, "../details", nil)
	requireHistoryMessage(t, err, "invalid artifact category")
	if _, err := s.DB.Exec("UPDATE sessions SET read_only=1 WHERE id=?", v.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.StartRequest(v.ID, turn, "main/child", "compaction", v.Model)
	requireHistoryMessage(t, err, "session is read-only")
	_, err = s.CommitChange(v.ID, "call", []string{}, false)
	requireHistoryMessage(t, err, "session is read-only")
}

func TestHistoryArtifactMessagesAndWrapping(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	for _, maxBytes := range []int64{-1, math.MaxInt64} {
		_, err := openArtifact("unused", maxBytes)
		requireHistoryMessage(t, err, "invalid saved artifact byte limit")
	}
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := readArtifact(path, 10)
	requireHistoryMessage(t, err, "saved artifact must be a regular file")
	data := []byte("original")
	path, err = s.Artifact(v.ID, "details", data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed!"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = s.Artifact(v.ID, "details", data)
	requireHistoryMessage(t, err, "artifact hash conflict")
	if err := os.WriteFile(path, []byte("too large!"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = s.Artifact(v.ID, "details", data)
	requireHistoryMessage(t, err, "read saved artifact: saved artifact exceeds 8-byte limit")
	if errors.Unwrap(err) == nil || errors.Unwrap(err).Error() != "saved artifact exceeds 8-byte limit" {
		t.Fatal("saved-artifact wrapping lost", err)
	}
	_, err = filepathHash(filepath.Dir(path))
	requireHistoryMessage(t, err, "compaction archive must be a regular file")
	archive, err := s.writeArchive(v.ID, []byte("Markdown"), []byte("JSONL"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = s.writeArchive(v.ID, []byte("Markdown"), []byte("JSONL"))
	requireHistoryMessage(t, err, "archive content conflict")
}

func TestHistoryAdmissionMessages(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	_, err := s.AdmitRequest(v.ID, turn, "main/child", v.Model, &llm.Message{Role: "user", Content: "wrong envelope"}, nil, nil)
	requireHistoryMessage(t, err, "runtime context must be a nonempty developer runtime message")
	_, err = s.AdmitRequest(v.ID, turn, "main/child", v.Model, nil, []llm.Message{{EventSeq: math.MaxInt64}}, nil)
	requireHistoryMessage(t, err, "notification is not a committed event at request cutoff")
	_, err = s.AdmitRequest(v.ID, turn, "main/child", v.Model, nil, nil, nil, llm.Message{Role: "user"})
	requireHistoryMessage(t, err, "only main requests accept human steering")
	_, err = s.AdmitRequest(v.ID, turn, "main", v.Model, nil, nil, nil, llm.Message{Role: "assistant"})
	requireHistoryMessage(t, err, "steering must be a human user message")
	_, err = s.AdmitRequest(v.ID, turn, "main/child", v.Model, nil, nil, []llm.Message{{Role: "tool", Content: fmt.Sprintf(`{"finish_event_seq":%d}`, int64(math.MaxInt64))}})
	requireHistoryMessage(t, err, "child finish exceeds request cutoff")
	if err := s.InvalidateContext(v.ID, "broken summary"); err != nil {
		t.Fatal(err)
	}
	_, err = s.AdmitRequest(v.ID, turn, "main/child", v.Model, nil, nil, nil)
	requireHistoryMessage(t, err, "session unusable after compaction: broken summary; start or load another session")
	requireHistoryMessage(t, s.InvalidateContext(v.ID, ""), "missing compaction failure reason")
	entries, err := s.Branch(v.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var event struct{ Text string }
	if err := json.Unmarshal(entries[len(entries)-1].Content, &event); err != nil || event.Text != "Context unusable after compaction: broken summary" {
		t.Fatal("compaction-failure archive text changed", event, err)
	}
}

func TestHistoryArchiveCaptions(t *testing.T) {
	s, v, turn, request := historyFixture(t)
	snapshot := filepath.Join(t.TempDir(), "snapshot>file.pdf")
	message, err := json.Marshal(llm.Message{Role: "assistant", Content: "Evidence", Files: []llm.BinaryFile{{Path: snapshot}}})
	if err != nil {
		t.Fatal(err)
	}
	text, err := s.ExportText(Entry{Kind: "message", Content: message})
	if err != nil || text != "Evidence\n\nBinary file: [snapshot](<"+strings.ReplaceAll(snapshot, ">", "\\>")+">)" {
		t.Fatalf("binary caption = %q, %v", text, err)
	}
	call, err := s.CallIntent(v.ID, turn, "main/child", request, llm.ToolCall{ID: "provider-call", Name: "read", Arguments: json.RawMessage(`{"path":"file"}`)})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := json.Marshal(map[string]string{"call_id": call})
	if err != nil {
		t.Fatal(err)
	}
	text, err = s.ExportText(Entry{Kind: "tool_call", Content: ref})
	if err != nil || !strings.HasPrefix(text, "Pending call:\n\n") {
		t.Fatalf("pending call caption = %q, %v", text, err)
	}
	pending := &llm.Message{Role: "assistant", Content: "Last partial reply"}
	path, err := s.ArchiveActorTranscript(v.ID, "main/other-child", pending)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	want := "# Child conversation · main/other-child\n\n### Uncommitted assistant output\n\nLast partial reply\n\n"
	if err != nil || string(data) != want {
		t.Fatalf("child archive = %q, %v; want %q", data, err, want)
	}
	_, err = s.ArchiveActorTranscript(v.ID, " ", nil)
	requireHistoryMessage(t, err, "actor transcript requires an actor")
	_, err = s.ArchiveActorTranscript(v.ID, "main/child", &llm.Message{Role: "user"})
	requireHistoryMessage(t, err, "pending transcript output must be an assistant message")
	_, err = s.ExportText(Entry{Kind: "status", Content: json.RawMessage(`{"type":"job_completion"}`)})
	requireHistoryMessage(t, err, "invalid job completion presentation")
	text, err = s.ExportText(Entry{Kind: "status", Content: json.RawMessage(`{"type":"request_message","purpose":"compaction","role":"assistant"}`)})
	if err != nil || text != "compaction assistant · inspect" {
		t.Fatalf("request message archive label = %q, %v", text, err)
	}
}

func TestHistoryInputMessagesPreserveWrapping(t *testing.T) {
	entry := Entry{ID: 7, Actor: "main", Kind: "message", Role: "user", Visible: true}
	message := llm.Message{Role: "user"}
	_, err := enrichInput(nil, entry, message)
	requireHistoryMessage(t, err, "original human input #7: sql: no rows in result set")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("missing-input identity lost", err)
	}
	_, err = enrichInput(map[int64]inputMetadata{7: {source: "invalid"}}, entry, message)
	requireHistoryMessage(t, err, `original human input #7 has invalid source "invalid"`)
	cause := errors.New("fixture cause")
	for _, format := range []string{prompts.HistoryCompactionValidateMarkdown, prompts.HistoryCompactionValidateExact, prompts.HistoryInputMetadata, prompts.HistoryNotificationSource} {
		if err := fmt.Errorf(format, cause); !errors.Is(err, cause) {
			t.Fatal("history error format lost cause", format, err)
		}
	}
}

// Child failures and authored archive text reach the parent indirectly. Scope
// the guard to reachable methods rather than extracting operator-only history
// commands, loading, inspection pagination or prompt-recall UI diagnostics.
func TestHistoryModelFacingDiagnosticsUsePromptAssets(t *testing.T) {
	functions := map[string][]string{
		"archive.go":   {"ArchiveMessages", "writeArchive"},
		"artifact.go":  {"readArtifact", "openArtifact"},
		"children.go":  {"FinishChildTurn", "validateChildFinish"},
		"compact.go":   {"Continue", "filepathHash"},
		"events.go":    {"AdmitRequest", "BeginChildTurn", "InvalidateContext"},
		"export.go":    {"transcriptEntries", "ExportText", "ArchiveActorTranscript", "actorTranscriptEntriesWith", "ArchiveTranscript", "transcriptJSONLEntries"},
		"history.go":   {"appendTx", "Append", "Messages", "messagesWith", "StartRequest", "FinishRequest", "CallIntent", "CallResult", "Artifact", "RecordSystemPrompt", "Assistant", "Inspect"},
		"inputs.go":    {"inputMetadataWith", "enrichInput"},
		"inspect.go":   {"RequestMessage"},
		"mutations.go": {"CommitChange"},
	}
	fset := token.NewFileSet()
	for path, names := range functions {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		selected := make(map[string]bool, len(names))
		for _, name := range names {
			selected[name] = true
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || !selected[function.Name.Name] {
				continue
			}
			delete(selected, function.Name.Name)
			ast.Inspect(function.Body, func(node ast.Node) bool {
				// These archive renderers have protocol tokens and Markdown
				// punctuation, but no authored prose literals. Check captions
				// too: they may use concatenation instead of an error sink.
				if path == "export.go" && (function.Name.Name == "transcriptEntries" || function.Name.Name == "ExportText" || function.Name.Name == "ArchiveActorTranscript") {
					if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING {
						text, err := strconv.Unquote(literal.Value)
						if err != nil {
							t.Fatal(err)
						}
						if !strings.HasPrefix(text, "SELECT ") && strings.ContainsAny(text, " \n\t") && strings.ContainsFunc(text, unicode.IsLetter) {
							t.Errorf("%s: authored archive caption belongs in prompt/history-messages.yaml", fset.Position(literal.Pos()))
						}
					}
				}
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				owner, ok := selector.X.(*ast.Ident)
				if !ok || !(owner.Name == "fmt" && selector.Sel.Name == "Errorf" || owner.Name == "errors" && selector.Sel.Name == "New") {
					return true
				}
				asset, ok := call.Args[0].(*ast.SelectorExpr)
				if ok {
					packageName, ok := asset.X.(*ast.Ident)
					if ok && packageName.Name == "prompts" {
						return true
					}
				}
				t.Errorf("%s: model-facing history diagnostic must use a static prompt constant", fset.Position(call.Pos()))
				return true
			})
		}
		if len(selected) != 0 {
			t.Errorf("%s: guarded methods missing: %v", path, selected)
		}
	}
}
