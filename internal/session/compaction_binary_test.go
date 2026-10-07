package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

func TestCompactionPreservesUnreadBinaryCycle(t *testing.T) {
	for _, actor := range []string{"main", "child", "btw"} {
		for _, fits := range []bool{true, false} {
			for _, trailingInput := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/fits=%v/trailing-input=%v", actor, fits, trailingInput), func(t *testing.T) {
					r, _ := runtimeFixture(t, nil)
					compactionBudget(t, r)
					seedRuntime(t, r, "Main research task")
					data := []byte("%PDF-1.4\n" + strings.Repeat("original document marker\n", 1000) + "%%EOF\n")
					path := filepath.Join(r.Workspace.Root, "unread.pdf")
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
					sum := sha256.Sum256(data)
					file := llm.BinaryFile{Path: path, SHA256: hex.EncodeToString(sum[:]), MIMEType: "application/pdf", Bytes: len(data)}
					if fits {
						r.selection.Model.Budget.ContextLimit += file.EstimatedTokens()
					}
					r.selection.Model.BinaryFiles = pdfCapability()
					messages := []llm.Message{
						{Role: "user", Content: "Earlier research instruction", InputTimeMS: time.Now().Add(-time.Minute).UnixMilli()},
						{Role: "assistant", Content: strings.Repeat("Older completed cycle. ", 1000)},
						{Role: "user", Content: "Read the original document", InputTimeMS: time.Now().Add(-time.Second).UnixMilli()},
						{Role: "assistant", Calls: []llm.ToolCall{{ID: "unread-document", Name: "read", Arguments: []byte(`{"path":"unread.pdf"}`)}}},
						{Role: "tool", CallID: "unread-document", Content: `{"ok":true,"kind":"document"}`, Files: []llm.BinaryFile{file}},
					}
					if trailingInput {
						messages = append(messages, llm.Message{Role: "user", Content: "Keep reading before answering", InputTimeMS: time.Now().UnixMilli()})
					}
					if contextbuild.Tokens(messages[3:]) <= r.selection.Model.Budget.RecentTokensMax {
						t.Fatal("fixture did not exceed the recent-cycle target")
					}
					before := r.Current()
					mainBefore, err := r.Store.Messages(before)
					if err != nil {
						t.Fatal(err)
					}
					assertNative := func(result []llm.Message) {
						t.Helper()
						calls, files, inputs := 0, 0, 0
						for _, message := range result {
							if strings.Contains(message.Content, "Older completed cycle.") {
								t.Fatal("older cycle was not compacted")
							}
							if message.Role == "user" && message.Content == "Keep reading before answering" {
								inputs++
							}
							for _, call := range message.Calls {
								if call.ID == "unread-document" {
									calls++
									if !reflect.DeepEqual(call, messages[3].Calls[0]) {
										t.Fatal("retained tool call changed", call)
									}
								}
							}
							if message.Role == "tool" && message.CallID == "unread-document" {
								files++
								if !reflect.DeepEqual(message, messages[4]) {
									t.Fatal("original document was replaced by metadata or summary", message)
								}
							}
						}
						if calls != 1 || files != 1 || inputs != map[bool]int{true: 1, false: 0}[trailingInput] {
							t.Fatal("protected cycle lost or duplicated", calls, files, inputs)
						}
					}
					summaries, coding := 0, 0
					r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
						if req.NoTools {
							summaries++
							input := req.Messages[0].Content
							if !strings.Contains(input, "Older completed cycle.") || strings.Contains(input, "unread.pdf") || strings.Contains(input, "unread-document") || strings.Contains(input, file.SHA256) {
								t.Fatal("summary consumed metadata instead of preserving the never-consumed original", input)
							}
							return emit(llm.StreamEvent{Kind: "text", Text: "Earlier research completed; inspect the retained original next."})
						}
						coding++
						if !fits {
							t.Fatal("coding request sent after unfittable protected cycle")
						}
						assertNative(req.Messages)
						if !contextbuild.Fits(req.Selection, req.System, req.Tools, req.Messages, false) {
							t.Fatal("oversized coding request admitted")
						}
						return emit(llm.StreamEvent{Kind: "text", Text: "Original document consumed."})
					}}
					var result []llm.Message
					cursor := contextCursor{project: "old project", snapshot: "old runtime"}
					if actor == "main" {
						for _, message := range messages {
							if _, err := r.Store.Append(before, "", "main", "message", message.Role, true, message); err != nil {
								t.Fatal(err)
							}
						}
						err = r.Run(nil)
						if err == nil {
							result, err = r.Store.Messages(r.Current())
						}
					} else {
						id := "main/" + actor + "_binary"
						turn, e := r.Store.BeginChildTurn(before, id, r.CurrentSelection())
						if e != nil {
							t.Fatal(e)
						}
						result, cursor, err = r.compactChild(context.Background(), childTask{actor: id, turn: turn, selection: r.CurrentSelection(), tools: r.Tools, aside: actor == "btw"}, messages, cursor, nil)
						mainAfter, e := r.Store.Messages(r.Current())
						if e != nil || r.Current() != before || !reflect.DeepEqual(mainAfter, mainBefore) {
							t.Fatal("isolated compaction changed main history", e)
						}
					}
					if summaries != 1 {
						t.Fatal("expected exactly one bounded summary request", summaries, err)
					}
					if !fits {
						if err == nil || !strings.Contains(err.Error(), prompts.SessionCompactionHeadroom) || r.Current() != before || result != nil || coding != 0 {
							t.Fatal("unfittable unread original was silently summarized", err, coding)
						}
						if actor != "main" && (cursor.project != "old project" || cursor.snapshot != "old runtime") {
							t.Fatal("failed isolated compaction changed cursor", cursor)
						}
						if actor == "main" {
							unchanged, e := r.Store.Messages(before)
							if e != nil {
								t.Fatal(e)
							}
							found := false
							for _, message := range unchanged {
								if reflect.DeepEqual(message, messages[4]) {
									found = true
								}
							}
							if !found || r.checkContext() == nil {
								t.Fatal("failed handoff lost original or left an unusable context admitted")
							}
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					assertNative(result)
					if actor == "main" && (coding != 1 || r.Current() == before) {
						t.Fatal("automatic compaction did not continue exactly once", coding)
					}
					if actor == "btw" {
						found := false
						for _, message := range result {
							found = found || message.Role == "developer" && message.Content == btwInstruction
						}
						if !found {
							t.Fatal("aside scope lost while preserving native original")
						}
					}
				})
			}
		}
	}
}
