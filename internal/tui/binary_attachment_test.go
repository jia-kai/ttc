package tui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/blobcache"
	"ttc/internal/llm"
	"ttc/internal/session"
)

type binaryAttachmentTestProvider struct {
	llm.Script
	requests chan llm.Request
}

func (p *binaryAttachmentTestProvider) Stream(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
	select {
	case p.requests <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	return p.Script.Stream(ctx, req, emit)
}

func TestPDFBinaryAttachmentHeadlessCompletionAndCommand(t *testing.T) {
	for _, method := range []string{"completion", "attach-command"} {
		t.Run(method, func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			p := &binaryAttachmentTestProvider{
				Script:   llm.Script{Responses: []llm.ScriptResponse{{Text: "Frozen PDF consumed."}, {Text: "Loaded PDF consumed."}}},
				requests: make(chan llm.Request, 2),
			}
			u := newQuestionTestUIWithSetup(t, p, nil, func(r *session.Runtime) {
				selection := r.CurrentSelection()
				// Applying a distinct catalog choice before the frontend starts
				// avoids concurrent mutation of its capability snapshot.
				selection.Model.ID = "scripted-pdf"
				selection.Model.BinaryFiles = []llm.BinaryFileType{{MIMEType: "application/pdf", Extensions: []string{".pdf"}, Kind: "document", MaxBytes: 32 << 20}}
				if err := r.RequestModel(selection); err != nil {
					t.Fatal(err)
				}
				if _, err := r.ApplyModel(""); err != nil {
					t.Fatal(err)
				}
			})
			path := filepath.Join(u.runtime.Workspace.Root, "research-evidence.pdf")
			data := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF\n")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			want := llm.BinaryFile{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), MIMEType: "application/pdf", Bytes: len(data)}
			authored := "Explain the attached document."
			if method == "completion" {
				u.typeText("Explain @research-evi")
				u.wait(t, "research-evidence.pdf")
				u.key(tcell.KeyTab)
				u.wait(t, "1 attachments")
				// Completion must remain visible in authored input; attachment
				// expansion is separate from the literal @ token.
				authored = "Explain @research-evidence.pdf please."
				u.typeText("please.")
			} else {
				u.typeText("/attach " + path)
				u.key(tcell.KeyEnter)
				u.wait(t, "1 attachments")
				u.typeText(authored)
			}
			if err := os.WriteFile(path, []byte("changed before submit"), 0600); err != nil {
				t.Fatal(err)
			}
			u.key(tcell.KeyEnter)
			frame := u.wait(t, "Turn complete")
			if !strings.Contains(frame, authored) || strings.Contains(frame, "Document attachment:") {
				t.Fatal("conversation did not preserve authored text separately", frame)
			}
			check := func(messages []llm.Message) {
				t.Helper()
				count := 0
				for _, m := range messages {
					if m.Role != "user" || m.Runtime || m.DisplayText() != authored {
						continue
					}
					count++
					if m.UserText == nil || *m.UserText != authored || !strings.HasPrefix(m.Content, authored) || !reflect.DeepEqual(m.Files, []llm.BinaryFile{want}) {
						t.Fatalf("provider/history lost frozen native PDF or authored input: %+v", m)
					}
					encoded, err := json.Marshal(m)
					if err != nil || bytes.Contains(encoded, []byte("data_url")) || bytes.Contains(encoded, []byte(base64.StdEncoding.EncodeToString(data))) {
						t.Fatal("provider/history inlined PDF bytes", string(encoded), err)
					}
				}
				if count != 1 {
					t.Fatalf("expected exactly one attached input, got %d", count)
				}
			}
			checkRequest := func() {
				t.Helper()
				select {
				case req := <-p.requests:
					check(req.Messages)
				case <-time.After(3 * time.Second):
					t.Fatal("provider did not receive attached input")
				}
			}
			checkRequest()
			original := u.runtime.Current()
			messages, err := u.runtime.Store.Messages(original)
			if err != nil {
				t.Fatal(err)
			}
			check(messages)
			exact, err := u.runtime.Store.TranscriptJSONL(original, 0)
			if err != nil || !bytes.Contains(exact, []byte(want.SHA256)) || bytes.Contains(exact, []byte("data_url")) {
				t.Fatal("durable exact history lost native PDF reference", err)
			}
			cache, err := blobcache.Default()
			if err != nil {
				t.Fatal(err)
			}
			cached, err := cache.Get(context.Background(), "original", want.SHA256)
			if err != nil || !bytes.Equal(cached, data) {
				t.Fatal("attachment did not freeze original PDF in shared cache", err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			const loadedName = "Reloaded native PDF fixture"
			if _, err := u.runtime.Store.DB.Exec("UPDATE sessions SET name=? WHERE id=?", loadedName, original); err != nil {
				t.Fatal(err)
			}
			u.typeText("/load " + original)
			u.key(tcell.KeyEnter)
			frame = u.wait(t, loadedName)
			if !strings.Contains(frame, authored) || strings.Contains(frame, "Document attachment:") {
				t.Fatal("reload expanded or lost authored input", frame)
			}
			messages, err = u.runtime.Store.Messages(u.runtime.Current())
			if err != nil {
				t.Fatal(err)
			}
			check(messages)
			// A new request after history load must resend the cached reference,
			// not parse @ text or reopen the now missing original file.
			u.typeText("Continue from the retained PDF.")
			u.key(tcell.KeyEnter)
			u.wait(t, "Loaded PDF consumed.")
			checkRequest()
		})
	}
}
