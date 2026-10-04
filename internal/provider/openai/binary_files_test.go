package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/blobcache"
	"ttc/internal/provider"
)

func TestDocumentFormatCatalog(t *testing.T) {
	want := map[string][]string{
		"application/pdf":    {".pdf"},
		"application/msword": {".doc", ".dot"},
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": {".docx"},
		"application/vnd.ms-excel": {".xla", ".xlb", ".xlc", ".xlm", ".xls", ".xlt", ".xlw"},
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         {".xlsx"},
		"application/vnd.ms-powerpoint":                                             {".pot", ".ppa", ".pps", ".ppt", ".pwz", ".wiz"},
		"application/vnd.openxmlformats-officedocument.presentationml.presentation": {".pptx"},
		"application/rtf": {".rtf"}, "text/rtf": {".rtf"},
		"application/vnd.oasis.opendocument.text": {".odt"},
		"application/vnd.apple.pages":             {".pages"}, "application/vnd.apple.keynote": {".key"},
		"application/vnd.apple.iwork": {".pages", ".key"},
	}
	formats := documentFileTypes()
	if len(formats) != len(want) {
		t.Fatal(formats)
	}
	for _, f := range formats {
		if !reflect.DeepEqual(want[f.MIMEType], f.Extensions) || f.Kind != "document" || f.MaxBytes != blobcache.MaxBytes {
			t.Fatalf("incorrect document format: %+v", f)
		}
		delete(want, f.MIMEType)
	}
	if len(want) != 0 {
		t.Fatal("missing formats", want)
	}
}

func TestNativeDocumentsInHumanAndToolContent(t *testing.T) {
	for _, format := range documentFileTypes() {
		for _, role := range []string{"user", "tool"} {
			t.Run(format.MIMEType+"/"+role, func(t *testing.T) {
				// Transport fixtures need no parser or installed office software:
				// bytes are preserved, not converted or extracted locally.
				data := []byte("original native document bytes")
				file := originalBinaryFile(t, "fixture"+format.Extensions[0], data)
				file.MIMEType, file.Bytes = format.MIMEType, len(data)
				selection := provider.Selection{Provider: "openai", Model: provider.ScriptModel()}
				selection.Model.BinaryFiles = documentFileTypes()
				m := provider.Message{Role: role, CallID: "read-document", Content: "metadata", Files: []provider.BinaryFile{file}}
				req := provider.Request{ConversationID: "documents", Selection: selection, Messages: []provider.Message{m}}
				body, err := wire(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				var decoded struct{ Input []map[string]any }
				if err := json.Unmarshal(body, &decoded); err != nil {
					t.Fatal(err)
				}
				field := "content"
				if role == "tool" {
					field = "output"
				}
				want := []any{map[string]any{"type": "input_text", "text": "metadata"}, map[string]any{"type": "input_file", "filename": "fixture" + format.Extensions[0], "file_data": "data:" + format.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(data)}}
				if len(decoded.Input) != 1 || !reflect.DeepEqual(decoded.Input[0][field], want) {
					t.Fatalf("wrong document transport: %s", body)
				}
				if role == "tool" && decoded.Input[0]["call_id"] != m.CallID {
					t.Fatal("tool association lost")
				}
				if file.DataURL != "" || !reflect.DeepEqual(req.Messages[0], m) {
					t.Fatal("canonical reference mutated")
				}
			})
		}
	}
}

func TestDetectedDocumentTransportFilename(t *testing.T) {
	for _, name := range []string{"download", "download.txt", "download.PDF"} {
		t.Run(name, func(t *testing.T) {
			file := originalBinaryFile(t, name, []byte("%PDF-1.4\n%%EOF\n"))
			file.MIMEType = "application/pdf"
			file.Bytes = len("%PDF-1.4\n%%EOF\n")
			model := provider.ScriptModel()
			model.BinaryFiles = documentFileTypes()
			part, err := binaryPart(context.Background(), file, model, "input_text")
			want := name
			if !strings.HasSuffix(strings.ToLower(name), ".pdf") {
				want += ".pdf"
			}
			if err != nil || part["filename"] != want || !strings.HasSuffix(file.Path, name) {
				t.Fatal("detected document has inconsistent filename", part, err)
			}
		})
	}
}

func TestDocumentCapabilitiesLimitsAndUnavailable(t *testing.T) {
	data := []byte("%PDF-1.4\n%%EOF")
	file := originalBinaryFile(t, "document.pdf", data)
	file.MIMEType, file.Bytes = "application/pdf", len(data)
	selection := provider.Selection{Provider: "openai", Model: provider.ScriptModel()}
	m := provider.Message{Role: "tool", CallID: "read", Files: []provider.BinaryFile{file}}
	req := provider.Request{ConversationID: "documents", Selection: selection, Messages: []provider.Message{m}}
	if _, err := wire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "does not support") {
		t.Fatal("unsupported document accepted", err)
	}
	req.Selection.Model.BinaryFiles = []provider.BinaryFileType{{MIMEType: "application/pdf", Kind: "document", MaxBytes: len(data) - 1}}
	if _, err := wire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "exceeds model limit") {
		t.Fatal("model limit bypassed", err)
	}
	req.Selection.Model.BinaryFiles = documentFileTypes()
	if err := os.Remove(file.Path); err != nil {
		t.Fatal(err)
	}
	body, err := wire(context.Background(), req)
	if err != nil || !strings.Contains(string(body), "binary file unavailable") || strings.Contains(string(body), "file_data") {
		t.Fatalf("missing original not explicit: %s, %v", body, err)
	}
	req.Messages[0].State = &provider.ReplayState{Provider: "openai", Model: selection.Model.RequestID(), Version: replayVersion, Items: []json.RawMessage{json.RawMessage(`{"type":"reasoning"}`)}}
	if _, err := wire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "canonical binary files") {
		t.Fatal("replay discarded document", err)
	}
}
