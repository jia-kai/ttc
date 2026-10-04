package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/blobcache"
	contextbuild "ttc/internal/context"
	"ttc/internal/provider"
)

func pdfCapability() []provider.BinaryFileType {
	return []provider.BinaryFileType{{MIMEType: "application/pdf", Extensions: []string{".pdf"}, Kind: "document", MaxBytes: 32 << 20}}
}

func TestBinaryReadFrozenCapabilitiesAndHistory(t *testing.T) {
	for _, supported := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsupported", true: "supported"}[supported], func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			data := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF\n")
			path := filepath.Join(r.Workspace.Root, "source.pdf")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if supported {
				r.selection.Model.BinaryFiles = pdfCapability()
			}
			calls := []provider.ToolCall{{ID: "pdf-call", Name: "read", Arguments: json.RawMessage(`{"path":"source.pdf"}`)}}
			turn, ids := batchIntents(t, r, "main", calls)
			// Tool execution must use admission, not the current model picker.
			if supported {
				r.selection.Model.BinaryFiles = nil
			} else {
				r.selection.Model.BinaryFiles = pdfCapability()
			}
			records, err := r.runToolBatch(context.Background(), turn, "main", r.Tools, calls, ids, nil)
			if err != nil || len(records) != 1 {
				t.Fatal(records, err)
			}
			if !supported {
				if len(records[0].Files) != 0 || !strings.Contains(string(records[0].Result), "unsupported_binary_input") {
					t.Fatalf("unsupported producing request accepted PDF: %+v", records[0])
				}
				return
			}
			sum := sha256.Sum256(data)
			want := provider.BinaryFile{Path: path, SHA256: hex.EncodeToString(sum[:]), MIMEType: "application/pdf", Bytes: len(data)}
			if !reflect.DeepEqual(records[0].Files, []provider.BinaryFile{want}) {
				t.Fatalf("lost original document identity: %+v", records[0])
			}
			cache, err := blobcache.Default()
			if err != nil {
				t.Fatal(err)
			}
			cached, err := cache.Get(context.Background(), "original", want.SHA256)
			if err != nil || string(cached) != string(data) {
				t.Fatal("document did not use shared original cache", err)
			}
			check := func(session string) {
				t.Helper()
				messages, err := r.Store.Messages(session)
				if err != nil {
					t.Fatal(err)
				}
				for _, message := range messages {
					if message.Role != "tool" || message.CallID != "pdf-call" {
						continue
					}
					if !reflect.DeepEqual(message.Files, []provider.BinaryFile{want}) || strings.Contains(message.Content, "base64") {
						t.Fatal("history changed binary reference", message)
					}
					without := message
					without.Files = nil
					if delta := contextbuild.Tokens([]provider.Message{message}) - contextbuild.Tokens([]provider.Message{without}); delta != want.EstimatedTokens() || delta <= 0 {
						t.Fatal("document reserve missing", delta)
					}
					return
				}
				t.Fatal("history lost PDF tool result")
			}
			original := r.Current()
			check(original)
			loaded, err := r.Store.Load(original)
			if err != nil {
				t.Fatal(err)
			}
			check(loaded.ID)
			archive, err := r.Store.ArchiveTranscript(original, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range []string{archive, archive + ".jsonl"} {
				body, err := os.ReadFile(file)
				if err != nil || !strings.Contains(string(body), want.SHA256) || strings.Contains(string(body), "base64") {
					t.Fatal("archive lost document reference or copied bytes", file, err)
				}
			}
		})
	}
}
