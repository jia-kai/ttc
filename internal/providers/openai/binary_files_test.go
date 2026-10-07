package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/binaryinput"
	"ttc/internal/llm"
)

func TestAdapterBinaryInputWithInMemoryResolver(t *testing.T) {
	data := []byte("in-memory original document bytes")
	file := llm.BinaryFile{Path: "memory.pdf", MIMEType: "application/pdf", Bytes: len(data)}
	selection := llm.Selection{Provider: "openai", Model: llm.ScriptModel()}
	selection.Model.BinaryFiles = documentFileTypes()
	req := llm.Request{ConversationID: "memory-binary", Selection: selection, NoTools: true, Messages: []llm.Message{{Role: "user", Files: []llm.BinaryFile{file}}}}
	// A nonexistent source and unusable cache root ensure no filesystem fallback.
	t.Setenv("XDG_CACHE_HOME", "/dev/null/no-cache")
	if _, err := wire(context.Background(), req, nil); err == nil || !strings.Contains(err.Error(), "configured resolver") {
		t.Fatal("missing resolver silently accepted binary input", err)
	}
	resolved := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		var decoded struct {
			Input      []map[string]any
			ToolChoice string `json:"tool_choice"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Error(err)
		}
		want := []any{map[string]any{"type": "input_file", "filename": "memory.pdf", "file_data": "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(data)}}
		if len(decoded.Input) != 1 || !reflect.DeepEqual(decoded.Input[0]["content"], want) || decoded.ToolChoice != "none" {
			t.Errorf("incorrect in-memory transport: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"memory-response\"}}\n\n")
	}))
	defer server.Close()
	a := NewAdapter(Config{Client: server.Client(), BaseURL: server.URL,
		TokenSource: func(context.Context) (AccessTokens, error) {
			return AccessTokens{Access: "memory", AccountID: "memory-account"}, nil
		},
		ResolveBinary: func(ctx context.Context, got llm.BinaryFile) (llm.BinaryPayload, error) {
			if err := ctx.Err(); err != nil {
				return llm.BinaryPayload{}, err
			}
			if got != file {
				t.Fatalf("reference changed: %+v", got)
			}
			resolved = true
			return llm.BinaryPayload{Data: data, MIMEType: "application/pdf"}, nil
		},
	})
	if err := a.Stream(context.Background(), req, func(llm.StreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !resolved {
		t.Fatal("adapter did not invoke injected resolver")
	}
}

func TestBinaryPartInjectedResolverFailuresAndLimits(t *testing.T) {
	model := llm.ScriptModel()
	model.BinaryFiles = []llm.BinaryFileType{{MIMEType: "application/pdf", Kind: "document", MaxBytes: 3}}
	file := llm.BinaryFile{Path: "memory.pdf", MIMEType: "application/pdf", Bytes: 3}
	resolve := func(context.Context, llm.BinaryFile) (llm.BinaryPayload, error) {
		return llm.BinaryPayload{Data: []byte("four"), MIMEType: "application/pdf"}, nil
	}
	if _, err := binaryPart(context.Background(), file, model, "input_text", resolve); err == nil || !strings.Contains(err.Error(), "exceeds model limit") {
		t.Fatal("resolved byte count bypassed model limit", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resolve = func(context.Context, llm.BinaryFile) (llm.BinaryPayload, error) {
		cancel()
		return llm.BinaryPayload{}, &llm.UnavailableBinaryFileError{File: file, Err: os.ErrNotExist}
	}
	if _, err := binaryPart(ctx, file, model, "input_text", resolve); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation degraded to an unavailable notice", err)
	}
}

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
		if !reflect.DeepEqual(want[f.MIMEType], f.Extensions) || f.Kind != "document" || f.MaxBytes != 32<<20 {
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
				selection := llm.Selection{Provider: "openai", Model: llm.ScriptModel()}
				selection.Model.BinaryFiles = documentFileTypes()
				m := llm.Message{Role: role, CallID: "read-document", Content: "metadata", Files: []llm.BinaryFile{file}}
				req := llm.Request{ConversationID: "documents", Selection: selection, Messages: []llm.Message{m}}
				body, err := wire(context.Background(), req, binaryinput.Resolve)
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
				if !reflect.DeepEqual(req.Messages[0], m) {
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
			model := llm.ScriptModel()
			model.BinaryFiles = documentFileTypes()
			part, err := binaryPart(context.Background(), file, model, "input_text", binaryinput.Resolve)
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
	selection := llm.Selection{Provider: "openai", Model: llm.ScriptModel()}
	m := llm.Message{Role: "tool", CallID: "read", Files: []llm.BinaryFile{file}}
	req := llm.Request{ConversationID: "documents", Selection: selection, Messages: []llm.Message{m}}
	if _, err := wire(context.Background(), req, binaryinput.Resolve); err == nil || !strings.Contains(err.Error(), "does not support") {
		t.Fatal("unsupported document accepted", err)
	}
	req.Selection.Model.BinaryFiles = []llm.BinaryFileType{{MIMEType: "application/pdf", Kind: "document", MaxBytes: len(data) - 1}}
	if _, err := wire(context.Background(), req, binaryinput.Resolve); err == nil || !strings.Contains(err.Error(), "exceeds model limit") {
		t.Fatal("model limit bypassed", err)
	}
	req.Selection.Model.BinaryFiles = documentFileTypes()
	if err := os.Remove(file.Path); err != nil {
		t.Fatal(err)
	}
	body, err := wire(context.Background(), req, binaryinput.Resolve)
	if err != nil || !strings.Contains(string(body), "binary file unavailable") || strings.Contains(string(body), "file_data") {
		t.Fatalf("missing original not explicit: %s, %v", body, err)
	}
	req.Messages[0].State = &llm.ReplayState{Provider: "openai", Model: selection.Model.RequestID(), Version: replayVersion, Items: []json.RawMessage{json.RawMessage(`{"type":"reasoning"}`)}}
	if _, err := wire(context.Background(), req, binaryinput.Resolve); err == nil || !strings.Contains(err.Error(), "canonical binary files") {
		t.Fatal("replay discarded document", err)
	}
}
