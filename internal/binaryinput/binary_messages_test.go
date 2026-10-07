package binaryinput

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// Pin the original bytes, including formatting verbs. Comparing only against
// the generated constants would also accept accidental edits to the assets.
func TestBinaryDiagnosticPromptBytes(t *testing.T) {
	for got, want := range map[string]string{
		prompts.BinaryOpenCache:              "open binary cache: %w",
		prompts.BinaryCacheFile:              "cache binary file: %w",
		prompts.BinaryPDFVersionHeader:       "missing PDF version header",
		prompts.BinaryPDFEndMarker:           "missing PDF end marker",
		prompts.BinaryRTFHeader:              "missing RTF header or closing brace",
		prompts.BinaryOLEHeader:              "missing OLE compound-file header",
		prompts.BinaryOLEVersion:             "invalid OLE version/sector size",
		prompts.BinaryOLESector:              "incomplete OLE sector",
		prompts.BinaryZIPDuplicateEntry:      "duplicate ZIP entry %q",
		prompts.BinaryIWorkIndex:             "missing iWork document index",
		prompts.BinaryZIPMissingEntry:        "missing ZIP entry %q",
		prompts.BinaryODTMIMETooLarge:        "oversized ODT MIME identifier",
		prompts.BinaryODTMIMEIncorrect:       "incorrect ODT MIME identifier",
		prompts.BinaryZIPLocalHeader:         "missing ZIP local-file header",
		prompts.BinaryZIPDirectoryBounds:     "ZIP directory exceeds 4096 entries/2 MiB, uses ZIP64, or is invalid",
		prompts.BinaryZIPDirectoryRecord:     "invalid ZIP directory record",
		prompts.BinaryZIPEntrySizeMismatch:   "ZIP directory entry count or size mismatch",
		prompts.BinaryZIPEntryCountMismatch:  "ZIP directory entry count mismatch",
		prompts.BinaryZIPEndRecord:           "missing ZIP end-of-directory record",
		prompts.BinaryInvalidImage:           "invalid image: %w",
		prompts.BinaryGIFBlockMarker:         "invalid GIF block marker %#x",
		prompts.BinaryReferenceSize:          "binary file size must be between 0 and %d bytes",
		prompts.BinaryReferenceMIME:          "binary file requires a canonical MIME type without parameters",
		prompts.BinaryReferenceRequired:      "file-backed binary requires an absolute path and SHA-256 checksum",
		prompts.BinaryReferencePath:          "binary file path must be absolute: %q",
		prompts.BinaryReferenceChecksum:      "binary file %q requires a lowercase hex SHA-256 checksum",
		prompts.BinaryDocumentSizeRequired:   "file-backed document requires original byte size",
		prompts.BinaryReferenceFailure:       "binary file %q: %w",
		prompts.BinaryOpenFileCache:          "open binary file cache: %w",
		prompts.BinaryReadCachedFile:         "read cached binary file: %w",
		prompts.BinaryReferenceSizeMismatch:  "binary file byte size differs from original",
		prompts.BinaryReferenceImageType:     "unsupported image type %q; document references require MIME type",
		prompts.BinaryReferenceMIMEMismatch:  "binary file MIME type differs from original image",
		prompts.BinaryCacheReconstructedFile: "cache reconstructed binary file: %w",
		prompts.BinarySourceRegularFile:      "source is not a regular file",
		prompts.BinarySourceTooLarge:         "source exceeds %d bytes",
		prompts.BinarySourceChecksumMismatch: "source SHA-256 checksum mismatch; original binary file has changed",
	} {
		if got != want {
			t.Errorf("diagnostic asset = %q, want %q", got, want)
		}
	}
}

func TestBinaryDocumentDiagnosticExactOutput(t *testing.T) {
	const docx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	const odt = "application/vnd.oasis.opendocument.text"
	ole := make([]byte, 512)
	copy(ole, oleMagic)
	ole[28], ole[29] = 0xfe, 0xff
	validOLE := bytes.Clone(ole)
	binary.LittleEndian.PutUint16(validOLE[26:28], 3)
	binary.LittleEndian.PutUint16(validOLE[30:32], 9)
	var duplicate bytes.Buffer
	zw := zip.NewWriter(&duplicate)
	for range 2 {
		if _, err := zw.Create("entry-%s\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, mime string
		data       []byte
		inner      string
	}{
		{"PDF header", "application/pdf", []byte("%PDF-bad"), "missing PDF version header"},
		{"PDF end", "application/pdf", []byte("%PDF-1.7"), "missing PDF end marker"},
		{"RTF", "application/rtf", []byte("{\\rtf1"), "missing RTF header or closing brace"},
		{"OLE header", "application/msword", []byte("bad"), "missing OLE compound-file header"},
		{"OLE version", "application/msword", ole, "invalid OLE version/sector size"},
		{"OLE sector", "application/msword", append(validOLE, 0), "incomplete OLE sector"},
		{"ZIP duplicate", docx, duplicate.Bytes(), "duplicate ZIP entry \"entry-%s\\n\""},
		{"ZIP missing", docx, zipFixture(t, map[string]string{"other": "data"}), "missing ZIP entry \"[Content_Types].xml\""},
		{"iWork index", "application/vnd.apple.pages", zipFixture(t, map[string]string{"other": "data"}), "missing iWork document index"},
		{"ODT MIME size", odt, zipFixture(t, map[string]string{"mimetype": strings.Repeat("x", 129), "content.xml": "", "META-INF/manifest.xml": ""}), "oversized ODT MIME identifier"},
		{"ODT MIME value", odt, zipFixture(t, map[string]string{"mimetype": "wrong", "content.xml": "", "META-INF/manifest.xml": ""}), "incorrect ODT MIME identifier"},
	} {
		t.Run(test.name, func(t *testing.T) {
			types := []llm.BinaryFileType{{Kind: "document", MIMEType: test.mime, MaxBytes: 1 << 20}}
			_, err := readBinary(context.Background(), bytes.NewReader(test.data), int64(len(test.data)), "document", test.mime, "", types)
			want := "unsupported_content: invalid " + test.mime + " document: " + test.inner + "; provide an original file matching its extension and announced MIME type"
			if err == nil || err.Error() != want {
				t.Fatalf("document diagnostic = %v, want %q", err, want)
			}
		})
	}
}

func TestBinaryZIPDiagnosticExactOutput(t *testing.T) {
	valid := zipFixture(t, map[string]string{"entry": "data"})
	central := int(binary.LittleEndian.Uint32(valid[len(valid)-6 : len(valid)-2]))
	for _, test := range []struct {
		name, want string
		mutate     func([]byte) []byte
	}{
		{"local header", "missing ZIP local-file header", func(data []byte) []byte { data[0] = 0; return data }},
		{"end record", "missing ZIP end-of-directory record", func(data []byte) []byte { return data[:len(data)-1] }},
		{"bounds", "ZIP directory exceeds 4096 entries/2 MiB, uses ZIP64, or is invalid", func(data []byte) []byte {
			binary.LittleEndian.PutUint16(data[len(data)-22+10:], 4097)
			return data
		}},
		{"record", "invalid ZIP directory record", func(data []byte) []byte { data[central] = 0; return data }},
		{"entry size", "ZIP directory entry count or size mismatch", func(data []byte) []byte {
			binary.LittleEndian.PutUint16(data[central+28:], 0xffff)
			return data
		}},
		{"entry count", "ZIP directory entry count mismatch", func(data []byte) []byte {
			binary.LittleEndian.PutUint16(data[len(data)-22+8:], 2)
			binary.LittleEndian.PutUint16(data[len(data)-22+10:], 2)
			return data
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := boundedDocumentZIP(test.mutate(bytes.Clone(valid)))
			if err == nil || err.Error() != test.want {
				t.Fatalf("ZIP diagnostic = %v, want %q", err, test.want)
			}
		})
	}
}

func TestBinaryReferenceDiagnosticExactOutputAndWrapping(t *testing.T) {
	checksum := strings.Repeat("a", 64)
	for _, test := range []struct {
		file llm.BinaryFile
		want string
	}{
		{llm.BinaryFile{Bytes: -1}, "binary file size must be between 0 and 33554432 bytes"},
		{llm.BinaryFile{MIMEType: "Application/PDF"}, "binary file requires a canonical MIME type without parameters"},
		{llm.BinaryFile{}, "file-backed binary requires an absolute path and SHA-256 checksum"},
		{llm.BinaryFile{Path: "relative-%s", SHA256: checksum}, "binary file path must be absolute: \"relative-%s\""},
		{llm.BinaryFile{Path: "/original-%s", SHA256: "bad"}, "binary file \"/original-%s\" requires a lowercase hex SHA-256 checksum"},
		{llm.BinaryFile{Path: "/original", SHA256: checksum, MIMEType: "application/pdf"}, "file-backed document requires original byte size"},
	} {
		_, err := Resolve(context.Background(), test.file)
		if err == nil || err.Error() != test.want {
			t.Errorf("reference diagnostic = %v, want %q", err, test.want)
		}
	}
	file := fileBinaryFile(t, []byte("original"))
	if err := os.Remove(file.Path); err != nil {
		t.Fatal(err)
	}
	_, err := Resolve(context.Background(), file)
	var unavailable *llm.UnavailableBinaryFileError
	var external *os.PathError
	if !errors.As(err, &unavailable) || !errors.As(err, &external) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reference failure lost wrapped external error: %v", err)
	}
	if want := fmt.Sprintf("binary file %q: %s", file.Path, unavailable.Error()); err.Error() != want {
		t.Fatalf("reference diagnostic = %q, want %q", err, want)
	}
	if want := fmt.Sprintf("open %s: %s", file.Path, external.Err); external.Error() != want {
		t.Fatalf("OS error payload was changed: %q, want %q", external.Error(), want)
	}
}

func TestBinaryImageDiagnosticExactOutputAndWrapping(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	// A complete PNG header passes DecodeConfig; missing pixel data reaches the
	// full decoder and its external error must remain wrapped, not rewritten.
	data := encoded.Bytes()[:33]
	_, err := validateReadImage(context.Background(), data)
	if !errors.Is(err, io.ErrUnexpectedEOF) || err.Error() != "invalid image: unexpected EOF" {
		t.Fatalf("image diagnostic lost bytes or wrapped decoder cause: %v", err)
	}
	gif := append([]byte("GIF89a\x01\x00\x01\x00\x00\x00\x00"), 0x7f)
	err = singleFrameGIF(context.Background(), gif)
	if err == nil || err.Error() != "invalid GIF block marker 0x7f" {
		t.Fatalf("GIF diagnostic = %v", err)
	}
	_, err = readBinary(context.Background(), bytes.NewReader(gif), int64(len(gif)), "image", "image/gif", "", (llm.ModelSpec{Images: true}).BinaryFileTypes())
	if err == nil || err.Error() != "unsupported_content: invalid GIF block marker 0x7f" {
		t.Fatalf("model-facing GIF diagnostic = %v", err)
	}
	for _, format := range []string{
		prompts.BinaryOpenCache, prompts.BinaryCacheFile, prompts.BinaryOpenFileCache,
		prompts.BinaryReadCachedFile, prompts.BinaryCacheReconstructedFile,
	} {
		cause := errors.New("external payload %s\nline two")
		wrapped := fmt.Errorf(format, cause)
		if !errors.Is(wrapped, cause) || !strings.HasSuffix(wrapped.Error(), ": "+cause.Error()) {
			t.Fatalf("format %q lost wrapped external payload: %v", format, wrapped)
		}
	}
}

// Binary failures can reach models through tool errors or unavailable original
// notices. Every authored error format must be a static prompt constant; its
// dynamic arguments and errors returned directly by libraries remain data.
func TestBinaryDiagnosticAssetsGuard(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		isPrompt := func(expression ast.Expr) bool {
			selector, ok := expression.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			owner, ok := selector.X.(*ast.Ident)
			return ok && owner.Name == "prompts"
		}
		var checkMessage func(ast.Expr)
		checkMessage = func(expression ast.Expr) {
			switch value := expression.(type) {
			case *ast.BasicLit:
				if value.Kind == token.STRING {
					t.Errorf("%s: authored binary message belongs in prompt/binary-messages.yaml", fset.Position(value.Pos()))
				}
			case *ast.BinaryExpr:
				checkMessage(value.X)
				checkMessage(value.Y)
			case *ast.ParenExpr:
				checkMessage(value.X)
			case *ast.CallExpr:
				if selector, ok := value.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Sprintf" && len(value.Args) > 0 && !isPrompt(value.Args[0]) {
					t.Errorf("%s: authored binary message format must be a prompt constant", fset.Position(value.Pos()))
				}
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.CallExpr:
				selector, ok := value.Fun.(*ast.SelectorExpr)
				if !ok || len(value.Args) == 0 {
					break
				}
				owner, ok := selector.X.(*ast.Ident)
				if ok && ((owner.Name == "fmt" && selector.Sel.Name == "Errorf") || (owner.Name == "errors" && selector.Sel.Name == "New")) && !isPrompt(value.Args[0]) {
					t.Errorf("%s: authored binary error format must be a prompt constant", fset.Position(value.Pos()))
				}
			case *ast.CompositeLit:
				if name, ok := value.Type.(*ast.Ident); ok && name.Name == "Error" {
					for _, element := range value.Elts {
						if field, ok := element.(*ast.KeyValueExpr); ok {
							if key, ok := field.Key.(*ast.Ident); ok && key.Name == "Message" {
								checkMessage(field.Value)
							}
						}
					}
				}
			}
			return true
		})
	}
}
