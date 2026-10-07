package context

import (
	"bytes"
	stdcontext "context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"ttc/internal/binaryinput"
	"ttc/internal/blobcache"
	"ttc/internal/llm"
)

func snapshotPDFTypes() []llm.BinaryFileType {
	return []llm.BinaryFileType{{MIMEType: "application/pdf", Extensions: []string{".pdf"}, Kind: "document", MaxBytes: blobcache.MaxBytes}}
}

func TestSnapshotNativeBinaryOriginals(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, kind, mime string
		data             []byte
		types            []llm.BinaryFileType
	}{
		{"source.pdf", "document", "application/pdf", []byte("%PDF-1.4\noriginal\n%%EOF\n"), snapshotPDFTypes()},
		// Image type comes from content, not the filename extension.
		{"pixels.data", "image", "image/png", pngBytes.Bytes(), (llm.ModelSpec{Images: true}).BinaryFileTypes()},
		{"large.pdf", "document", "application/pdf", append(append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte("x"), 9<<20)...), []byte("\n%%EOF\n")...), snapshotPDFTypes()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tt.name)
			if err := os.WriteFile(path, tt.data, 0600); err != nil {
				t.Fatal(err)
			}
			a, err := Snapshot(stdcontext.Background(), path, tt.types)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(tt.data)
			if a.Kind != tt.kind || a.File == nil || a.Text != "" || a.Truncated || a.File.Path != path || a.File.SHA256 != hex.EncodeToString(sum[:]) || a.File.MIMEType != tt.mime || a.File.Bytes != len(tt.data) {
				t.Fatalf("wrong binary snapshot: %+v", a)
			}
			encoded, err := json.Marshal(a.File)
			if err != nil || bytes.Contains(encoded, []byte("data_url")) || bytes.Contains(encoded, []byte("base64")) {
				t.Fatal("snapshot persisted inline binary data", string(encoded), err)
			}
			cache, err := blobcache.Default()
			if err != nil {
				t.Fatal(err)
			}
			original, err := cache.Get(stdcontext.Background(), "original", a.File.SHA256)
			if err != nil || !bytes.Equal(original, tt.data) {
				t.Fatal("original blob missing", err)
			}
			if err := os.WriteFile(path, []byte("replaced"), 0600); err != nil {
				t.Fatal(err)
			}
			payload, err := binaryinput.Resolve(stdcontext.Background(), *a.File)
			if err != nil || payload.MIMEType != tt.mime || !bytes.Equal(payload.Data, tt.data) {
				t.Fatal("changed source invalidated snapshot", err)
			}
			m := (Input{Text: "inspect @" + tt.name, Attachments: []Attachment{a}}).Message()
			if m.DisplayText() != "inspect @"+tt.name || len(m.Files) != 1 || m.Files[0] != *a.File || strings.Contains(m.Content, "base64") || !strings.Contains(m.Content, path) {
				t.Fatal("native message lost authored text or file reference", m)
			}
		})
	}
}

func TestSnapshotBinaryFailures(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	valid := []byte("%PDF-1.4\noriginal\n%%EOF\n")
	for _, tt := range []struct {
		name, want string
		data       []byte
		types      []llm.BinaryFileType
	}{
		{"unsupported.pdf", "unsupported_binary_input", valid, nil},
		{"corrupt.pdf", "unsupported_content", []byte("%PDF-1.4\nmissing end"), snapshotPDFTypes()},
		{"limit.pdf", "binary_too_large", valid, []llm.BinaryFileType{{MIMEType: "application/pdf", Extensions: []string{".pdf"}, Kind: "document", MaxBytes: len(valid) - 1}}},
		{"invalid.pdf", "invalid_binary_capability", valid, []llm.BinaryFileType{{MIMEType: "application/pdf", Extensions: []string{".pdf"}, Kind: "document"}}},
		{"uppercase.pdf", "invalid_binary_capability", valid, []llm.BinaryFileType{{MIMEType: "Application/PDF", Extensions: []string{".pdf"}, Kind: "document", MaxBytes: blobcache.MaxBytes}}},
		{"parameters.pdf", "invalid_binary_capability", valid, []llm.BinaryFileType{{MIMEType: "application/pdf; charset=binary", Extensions: []string{".pdf"}, Kind: "document", MaxBytes: blobcache.MaxBytes}}},
		{"empty.native", "unsupported_content", nil, []llm.BinaryFileType{{MIMEType: "application/x-native", Extensions: []string{".native"}, Kind: "document", MaxBytes: blobcache.MaxBytes}}},
		{"corrupt.png", "unsupported_content", []byte("\x89PNG\r\n\x1a\n"), (llm.ModelSpec{Images: true}).BinaryFileTypes()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tt.name)
			if err := os.WriteFile(path, tt.data, 0600); err != nil {
				t.Fatal(err)
			}
			a, err := Snapshot(stdcontext.Background(), path, tt.types)
			if err == nil || !strings.Contains(err.Error(), tt.want) || a.File != nil {
				t.Fatalf("failure carried native file: %+v, %v", a, err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "source.pdf")
	if err := os.WriteFile(path, valid, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	cancel()
	if a, err := Snapshot(ctx, path, snapshotPDFTypes()); !errors.Is(err, stdcontext.Canceled) || a.File != nil {
		t.Fatal("cancelled snapshot accepted", a, err)
	}
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", blocked)
	if a, err := Snapshot(stdcontext.Background(), path, snapshotPDFTypes()); err == nil || !strings.Contains(err.Error(), "open binary cache") || a.File != nil {
		t.Fatal("cache failure carried native file", a, err)
	}
}

func TestSnapshotBinaryUsesOpenedDescriptor(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "source.pdf")
	data := []byte("%PDF-1.4\noriginal\n%%EOF\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := snapshotOpened(stdcontext.Background(), path, f, snapshotPDFTypes())
	if err != nil || a.File == nil || a.File.Bytes != len(data) {
		t.Fatal("binary snapshot reopened replacement", a, err)
	}
}
