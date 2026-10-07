package session

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

	"ttc/internal/blobcache"
	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
)

func TestBinaryAttachmentFrozenNativeInputAndHistory(t *testing.T) {
	for _, format := range []string{"pdf", "png"} {
		t.Run(format, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			// Keep the shared cache private even when this test is run alone.
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			data := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF\n")
			mime := "application/pdf"
			r.selection.Model.BinaryFiles = pdfCapability()
			if format == "png" {
				var err error
				data, err = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
				if err != nil {
					t.Fatal(err)
				}
				mime = "image/png"
				r.selection.Model.Images = true
			}
			path := filepath.Join(r.Workspace.Root, "evidence."+format)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			attachment, err := contextbuild.Snapshot(context.Background(), path, r.CurrentSelection().Model.BinaryFileTypes())
			if err != nil {
				t.Fatal(err)
			}
			want := llm.BinaryFile{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), MIMEType: mime, Bytes: len(data)}
			kind := "document"
			if format == "png" {
				kind = "image"
			}
			if attachment.Kind != kind || attachment.File == nil || !reflect.DeepEqual(*attachment.File, want) || attachment.Text != "" {
				t.Fatalf("snapshot is not a native %s reference: %+v", kind, attachment)
			}
			if err := os.WriteFile(path, []byte("changed after attaching"), 0600); err != nil {
				t.Fatal(err)
			}
			const authored = "Inspect @evidence and preserve this authored input."
			input := contextbuild.Input{Text: authored, Attachments: []contextbuild.Attachment{attachment}}
			message := input.Message()
			check := func(messages []llm.Message) {
				t.Helper()
				count := 0
				for _, m := range messages {
					if m.Role != "user" || m.Runtime || m.DisplayText() != authored {
						continue
					}
					count++
					if m.UserText == nil || *m.UserText != authored || m.Content != message.Content || !reflect.DeepEqual(m.Files, []llm.BinaryFile{want}) {
						t.Fatalf("native authored attachment changed: %+v", m)
					}
					body, err := json.Marshal(m)
					if err != nil || bytes.Contains(body, []byte("data_url")) || bytes.Contains(body, []byte(base64.StdEncoding.EncodeToString(data))) {
						t.Fatal("attachment bytes were inlined", string(body), err)
					}
				}
				if count != 1 {
					t.Fatalf("expected one attached user input, got %d", count)
				}
			}
			check([]llm.Message{message})
			requests := 0
			script := &llm.Script{Responses: []llm.ScriptResponse{{Text: "Native attachment consumed."}, {Text: "Retained attachment consumed."}}}
			r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				requests++
				check(req.Messages)
				return script.Stream(ctx, req, emit)
			}}
			if err := r.RunInput(r.PrepareInput(input)); err != nil {
				t.Fatal(err)
			}
			original := r.Current()
			messages, err := r.Store.Messages(original)
			if err != nil {
				t.Fatal(err)
			}
			check(messages)
			entries, err := r.Store.Branch(original, 0)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, entry := range entries {
				if entry.Role == "user" && entry.Visible && bytes.Contains(entry.Content, []byte(want.SHA256)) {
					found = true
					if bytes.Contains(entry.Content, []byte("data_url")) || bytes.Contains(entry.Content, []byte(base64.StdEncoding.EncodeToString(data))) {
						t.Fatal("durable attachment history inlined original bytes")
					}
				}
			}
			if !found {
				t.Fatal("durable history lost attachment reference")
			}
			// Reload through the runtime and resend retained history, with no new
			// input or original file to read.
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Command("/load " + original); err != nil {
				t.Fatal(err)
			}
			messages, err = r.Store.Messages(r.Current())
			if err != nil {
				t.Fatal(err)
			}
			check(messages)
			if err := r.Run(nil); err != nil {
				t.Fatal(err)
			}
			if requests != 2 {
				t.Fatal("did not resend loaded attachment", requests)
			}
			cache, err := blobcache.Default()
			if err != nil {
				t.Fatal(err)
			}
			cached, err := cache.Get(context.Background(), "original", want.SHA256)
			if err != nil || !bytes.Equal(cached, data) {
				t.Fatal("frozen original missing from shared cache", err)
			}
			archive, err := r.Store.TranscriptJSONL(r.Current(), 0)
			if err != nil || !strings.Contains(string(archive), want.SHA256) || bytes.Contains(archive, []byte("data_url")) {
				t.Fatal("exact export lost native reference", err)
			}
		})
	}
}
