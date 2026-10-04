package tool

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"ttc/internal/assets"
	"ttc/internal/blobcache"
	"ttc/internal/provider"
)

func documentCapability(extension string) provider.BinaryFileType {
	mime := map[string]string{
		".pdf": "application/pdf", ".doc": "application/msword",
		".dot":  "application/msword",
		".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		".ppt":  "application/vnd.ms-powerpoint", ".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		".xls": "application/vnd.ms-excel", ".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		".xla": "application/vnd.ms-excel", ".xlb": "application/vnd.ms-excel", ".xlc": "application/vnd.ms-excel", ".xlm": "application/vnd.ms-excel", ".xlt": "application/vnd.ms-excel", ".xlw": "application/vnd.ms-excel",
		".pot": "application/vnd.ms-powerpoint", ".ppa": "application/vnd.ms-powerpoint", ".pps": "application/vnd.ms-powerpoint", ".pwz": "application/vnd.ms-powerpoint", ".wiz": "application/vnd.ms-powerpoint",
		".rtf": "application/rtf", ".odt": "application/vnd.oasis.opendocument.text",
		".pages": "application/vnd.apple.pages", ".key": "application/vnd.apple.keynote",
	}[extension]
	return provider.BinaryFileType{MIMEType: mime, Extensions: []string{extension}, Kind: "document", MaxBytes: assets.MaxBytes}
}

func zipFixture(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for name, content := range entries {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

// Fixtures check only the cheap format identity promised by read, not complete
// application-level validity; document interpretation belongs to the provider.
func documentFixture(t *testing.T, extension string) []byte {
	t.Helper()
	switch extension {
	case ".pdf":
		return []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF\n")
	case ".rtf":
		return []byte("{\\rtf1\\ansi synthetic marker}\n")
	case ".doc", ".dot", ".ppt", ".pot", ".ppa", ".pps", ".pwz", ".wiz", ".xls", ".xla", ".xlb", ".xlc", ".xlm", ".xlt", ".xlw":
		data := make([]byte, 1024)
		copy(data, oleMagic)
		binary.LittleEndian.PutUint16(data[26:28], 3)
		binary.LittleEndian.PutUint16(data[28:30], 0xfffe)
		binary.LittleEndian.PutUint16(data[30:32], 9)
		return data
	case ".docx":
		return zipFixture(t, map[string]string{"[Content_Types].xml": "<Types/>", "word/document.xml": "<document/>"})
	case ".pptx":
		return zipFixture(t, map[string]string{"[Content_Types].xml": "<Types/>", "ppt/presentation.xml": "<presentation/>"})
	case ".xlsx":
		return zipFixture(t, map[string]string{"[Content_Types].xml": "<Types/>", "xl/workbook.xml": "<workbook/>"})
	case ".odt":
		return zipFixture(t, map[string]string{"mimetype": "application/vnd.oasis.opendocument.text", "content.xml": "<document/>", "META-INF/manifest.xml": "<manifest/>"})
	case ".pages", ".key":
		return zipFixture(t, map[string]string{"Index/Document.iwa": "marker", "Metadata/Properties.plist": "<plist/>"})
	default:
		t.Fatalf("no fixture for %s", extension)
		return nil
	}
}

func TestReadDocumentsPreserveOriginalBytesAndReferences(t *testing.T) {
	for _, extension := range []string{".pdf", ".doc", ".dot", ".docx", ".ppt", ".pot", ".ppa", ".pps", ".pwz", ".wiz", ".pptx", ".xls", ".xla", ".xlb", ".xlc", ".xlm", ".xlt", ".xlw", ".xlsx", ".rtf", ".odt", ".pages", ".key"} {
		t.Run(extension, func(t *testing.T) {
			r, w, x, req := toolFixture(t)
			x.BinaryFiles = []provider.BinaryFileType{documentCapability(extension)}
			data := documentFixture(t, extension)
			name := "source" + strings.ToUpper(extension)
			path := filepath.Join(w.Root, name)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			args, _ := json.Marshal(map[string]string{"path": name})
			record := invoke(t, r, w, x, req, "read", string(args))
			ok(t, record)
			hash := sha256.Sum256(data)
			checksum := hex.EncodeToString(hash[:])
			want := provider.BinaryFile{Path: path, SHA256: checksum, MIMEType: x.BinaryFiles[0].MIMEType, Bytes: len(data)}
			if !reflect.DeepEqual(record.Files, []provider.BinaryFile{want}) {
				t.Fatalf("wrong binary references: %+v", record.Files)
			}
			var metadata map[string]any
			if err := json.Unmarshal(record.Result, &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata["kind"] != "document" || metadata["path"] != path || metadata["mime_type"] != want.MIMEType || metadata["sha256"] != checksum || metadata["bytes"] != float64(len(data)) || metadata["truncated"] != false || metadata["width"] != nil || metadata["content"] != nil || metadata["next_offset"] != nil {
				t.Fatalf("wrong document metadata: %s", record.Result)
			}
			cache, err := blobcache.Default()
			if err != nil {
				t.Fatal(err)
			}
			cached, err := cache.Get(context.Background(), "original", checksum)
			if err != nil || !bytes.Equal(cached, data) {
				t.Fatalf("cache did not retain original: %v", err)
			}
			version, encoded, err := record.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(encoded, []byte(`"files"`)) || bytes.Contains(encoded, []byte(`"images"`)) || bytes.Contains(encoded, []byte("data_url")) || len(encoded) > 4096 {
				t.Fatalf("record stores wrong transport or bytes: %s", encoded)
			}
			codec, _ := r.Get("read")
			decoded, err := codec.DecodeRecord(version, encoded)
			if err != nil || !reflect.DeepEqual(decoded, record) {
				t.Fatalf("record round trip failed: %v", err)
			}
			original, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(original, data) {
				t.Fatalf("source modified: %v", err)
			}
		})
	}
}

func TestReadDocumentsCapabilitiesPaginationAndText(t *testing.T) {
	r, w, x, req := toolFixture(t)
	pdf := documentFixture(t, ".pdf")
	for _, name := range []string{"source.pdf", "no-extension"} {
		if err := os.WriteFile(filepath.Join(w.Root, name), pdf, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"source.pdf", "no-extension"} {
		args, _ := json.Marshal(map[string]string{"path": name})
		for _, types := range [][]provider.BinaryFileType{nil, (provider.ModelSpec{Images: true}).BinaryFileTypes(), {documentCapability(".docx")}} {
			x.BinaryFiles = types
			record := invoke(t, r, w, x, req, "read", string(args))
			if len(record.Files) != 0 || !strings.Contains(string(record.Result), `"code":"unsupported_binary_input"`) || !strings.Contains(string(record.Result), "select a model") {
				t.Fatalf("ASCII PDF incorrectly read as text or without capability: %s", record.Result)
			}
		}
		x.BinaryFiles = []provider.BinaryFileType{documentCapability(".pdf")}
		ok(t, invoke(t, r, w, x, req, "read", string(args)))
		for _, pagination := range []string{"offset", "limit"} {
			args, _ := json.Marshal(map[string]any{"path": name, pagination: 1})
			record := invoke(t, r, w, x, req, "read", string(args))
			if len(record.Files) != 0 || !strings.Contains(string(record.Result), `"code":"invalid_input"`) || !strings.Contains(string(record.Result), "omit pagination") {
				t.Fatalf("binary pagination accepted: %s", record.Result)
			}
		}
	}
	for extension, mime := range map[string]string{".txt": "text/plain", ".csv": "text/csv", ".md": "text/markdown", ".json": "application/json", ".xml": "application/xml"} {
		name := "text" + extension
		if err := os.WriteFile(filepath.Join(w.Root, name), []byte("first\nsecond\n"), 0600); err != nil {
			t.Fatal(err)
		}
		x.BinaryFiles = []provider.BinaryFileType{{Kind: "document", MIMEType: mime, Extensions: []string{extension}, MaxBytes: assets.MaxBytes}}
		args, _ := json.Marshal(map[string]any{"path": name, "limit": 1})
		record := invoke(t, r, w, x, req, "read", string(args))
		ok(t, record)
		if len(record.Files) != 0 || !strings.Contains(string(record.Result), `"next_offset":2`) || !strings.Contains(string(record.Result), `"content":"first\n"`) {
			t.Fatalf("announced text format bypassed pagination: %s", record.Result)
		}
	}
}

func TestReadDocumentsRejectInvalidContainers(t *testing.T) {
	r, w, x, req := toolFixture(t)
	for _, extension := range []string{".pdf", ".doc", ".docx", ".ppt", ".pptx", ".xls", ".xlsx", ".rtf", ".odt", ".pages", ".key"} {
		x.BinaryFiles = []provider.BinaryFileType{documentCapability(extension)}
		name := "mislabeled" + extension
		if err := os.WriteFile(filepath.Join(w.Root, name), []byte("ordinary text\n"), 0600); err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]string{"path": name})
		record := invoke(t, r, w, x, req, "read", string(args))
		if len(record.Files) != 0 || !strings.Contains(string(record.Result), `"code":"unsupported_content"`) || !strings.Contains(string(record.Result), "matching its extension") {
			t.Fatalf("mislabeled %s accepted: %s", extension, record.Result)
		}
	}
	for name, data := range map[string][]byte{
		"missing.pdf":      []byte("%PDF-1.7\nmissing end marker"),
		"missing.docx":     zipFixture(t, map[string]string{"[Content_Types].xml": "<Types/>"}),
		"spreadsheet.docx": documentFixture(t, ".xlsx"),
		"wrong.odt":        zipFixture(t, map[string]string{"content.xml": "x", "META-INF/manifest.xml": "x", "mimetype": "text/plain"}),
		"bomb.odt":         zipFixture(t, map[string]string{"content.xml": "x", "META-INF/manifest.xml": "x", "mimetype": strings.Repeat("x", 1<<20)}),
		"truncated.docx":   documentFixture(t, ".docx")[:20],
	} {
		x.BinaryFiles = []provider.BinaryFileType{documentCapability(filepath.Ext(name))}
		if err := os.WriteFile(filepath.Join(w.Root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]string{"path": name})
		record := invoke(t, r, w, x, req, "read", string(args))
		if len(record.Files) != 0 || !strings.Contains(string(record.Result), `"code":"unsupported_content"`) {
			t.Fatalf("invalid document accepted: %s", record.Result)
		}
	}
	// Unannounced ZIP/OLE containers must never fall back to text.
	for name, data := range map[string][]byte{"unknown.zip": documentFixture(t, ".docx"), "unknown.bin": documentFixture(t, ".doc")} {
		if err := os.WriteFile(filepath.Join(w.Root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		x.BinaryFiles = []provider.BinaryFileType{documentCapability(".docx"), documentCapability(".doc")}
		args, _ := json.Marshal(map[string]string{"path": name})
		record := invoke(t, r, w, x, req, "read", string(args))
		if len(record.Files) != 0 || !strings.Contains(string(record.Result), `"code":"unsupported_binary_input"`) {
			t.Fatalf("ambiguous binary format accepted: %s", record.Result)
		}
	}
}

func TestReadUsesAnnouncedCatalogForNewFormatsAndMIMEValidation(t *testing.T) {
	r, w, x, req := toolFixture(t)
	data := []byte("FOOBIN\x00\xfforiginal opaque payload")
	capability := provider.BinaryFileType{Kind: "document", MIMEType: "application/vnd.example.foo", Extensions: []string{"FOO"}, MaxBytes: 1024}
	x.BinaryFiles = []provider.BinaryFileType{capability}
	path := filepath.Join(w.Root, "source.FoO")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	record := invoke(t, r, w, x, req, "read", `{"path":"source.FoO"}`)
	ok(t, record)
	hash := sha256.Sum256(data)
	checksum := hex.EncodeToString(hash[:])
	want := provider.BinaryFile{Path: path, SHA256: checksum, MIMEType: capability.MIMEType, Bytes: len(data)}
	if !reflect.DeepEqual(record.Files, []provider.BinaryFile{want}) || !strings.Contains(string(record.Result), `"kind":"document"`) {
		t.Fatalf("new provider-announced format did not flow through: %s, %+v", record.Result, record.Files)
	}
	cache, err := blobcache.Default()
	if err != nil {
		t.Fatal(err)
	}
	cached, err := cache.Get(context.Background(), "original", checksum)
	if err != nil || !bytes.Equal(cached, data) {
		t.Fatalf("opaque format was modified before caching: %v", err)
	}
	for _, maximum := range []int{0, -1} {
		x.BinaryFiles[0].MaxBytes = maximum
		record := invoke(t, r, w, x, req, "read", `{"path":"source.FoO"}`)
		if len(record.Files) != 0 || !strings.Contains(string(record.Result), `"code":"invalid_binary_capability"`) || !strings.Contains(string(record.Result), "positive MaxBytes") {
			t.Fatalf("invalid provider byte limit silently defaulted: %s", record.Result)
		}
	}
	for _, test := range []struct{ declared, actual string }{
		{".pdf", ".doc"}, {".doc", ".pdf"}, {".docx", ".xlsx"}, {".rtf", ".pdf"},
	} {
		capability := documentCapability(test.declared)
		// A new extension for a known MIME family must still get that family's
		// validation, rather than either hardcoded extension validation or none.
		capability.Extensions = []string{".foo"}
		x.BinaryFiles = []provider.BinaryFileType{capability}
		if err := os.WriteFile(path, documentFixture(t, test.actual), 0600); err != nil {
			t.Fatal(err)
		}
		record := invoke(t, r, w, x, req, "read", `{"path":"source.FoO"}`)
		if len(record.Files) != 0 || !strings.Contains(string(record.Result), `"code":"unsupported_content"`) {
			t.Fatalf("mismatched %s MIME accepted %s bytes: %s", capability.MIMEType, test.actual, record.Result)
		}
		if err := os.WriteFile(path, documentFixture(t, test.declared), 0600); err != nil {
			t.Fatal(err)
		}
		ok(t, invoke(t, r, w, x, req, "read", `{"path":"source.FoO"}`))
	}
}

func TestRTFTextMIMEUsesNativeBinaryRead(t *testing.T) {
	r, w, x, req := toolFixture(t)
	capability := documentCapability(".rtf")
	capability.MIMEType = "text/rtf"
	x.BinaryFiles = []provider.BinaryFileType{capability}
	for _, name := range []string{"source.rtf", "extensionless"} {
		if err := os.WriteFile(filepath.Join(w.Root, name), documentFixture(t, ".rtf"), 0600); err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]string{"path": name})
		record := invoke(t, r, w, x, req, "read", string(args))
		ok(t, record)
		if len(record.Files) != 1 || record.Files[0].MIMEType != "text/rtf" {
			t.Fatalf("text/rtf incorrectly treated as paginated text: %s", record.Result)
		}
		x.BinaryFiles = nil
		record = invoke(t, r, w, x, req, "read", string(args))
		if len(record.Files) != 0 || !strings.Contains(string(record.Result), `"code":"unsupported_binary_input"`) {
			t.Fatalf("unannounced ASCII RTF fell through to text: %s", record.Result)
		}
		x.BinaryFiles = []provider.BinaryFileType{capability}
	}
}

func TestReadDocumentBoundsCancellationCacheAndDescriptor(t *testing.T) {
	r, w, x, req := toolFixture(t)
	data := documentFixture(t, ".pdf")
	path := filepath.Join(w.Root, "source.pdf")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	capability := documentCapability(".pdf")
	capability.MaxBytes = len(data) - 1
	x.BinaryFiles = []provider.BinaryFileType{capability}
	record := invoke(t, r, w, x, req, "read", `{"path":"source.pdf"}`)
	if len(record.Files) != 0 || !strings.Contains(string(record.Result), `"code":"binary_too_large"`) {
		t.Fatalf("declared lower bound ignored: %s", record.Result)
	}
	// Simulate a descriptor that grew after Stat by supplying its earlier size.
	if _, err := readBinary(context.Background(), bytes.NewReader(data), 0, "document", "", ".pdf", x.BinaryFiles); err == nil || !strings.Contains(err.Error(), "binary_too_large") {
		t.Fatalf("actual byte bound ignored: %v", err)
	}
	capability.MaxBytes = assets.MaxBytes * 2
	x.BinaryFiles = []provider.BinaryFileType{capability}
	if err := os.Truncate(path, assets.MaxBytes+1); err != nil {
		t.Fatal(err)
	}
	record = invoke(t, r, w, x, req, "read", `{"path":"source.pdf"}`)
	if len(record.Files) != 0 || !strings.Contains(string(record.Result), "32 MiB") {
		t.Fatalf("global binary byte bound ignored: %s", record.Result)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	record = r.Invoke(ctx, x, "read", json.RawMessage(`{"path":"source.pdf"}`))
	if len(record.Files) != 0 || !strings.Contains(string(record.Result), `"code":"cancelled"`) {
		t.Fatalf("canceled document read accepted: %s", record.Result)
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
	value, err := readOpenedPage(context.Background(), f, path, 1, 200, x.BinaryFiles)
	if err != nil {
		t.Fatal(err)
	}
	binary, ok := value.(binaryRead)
	if !ok || !bytes.Equal(binary.data, data) {
		t.Fatal("document read reopened replacement instead of descriptor")
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", blocked)
	// Use the preserved regular file, not the FIFO replacement.
	record = invoke(t, r, w, x, req, "read", `{"path":"source.pdf.old"}`)
	if len(record.Files) != 0 || !strings.Contains(string(record.Result), "open binary cache") {
		t.Fatalf("cache failure carried an attachment: %s", record.Result)
	}
}

func TestDocumentZIPMetadataIsBounded(t *testing.T) {
	entries := make(map[string]string, 4097)
	for i := 0; i < 4097; i++ {
		entries[fmt.Sprintf("entry-%d", i)] = "x"
	}
	if _, err := boundedDocumentZIP(zipFixture(t, entries)); err == nil || !strings.Contains(err.Error(), "4096") {
		t.Fatalf("unbounded ZIP directory accepted: %v", err)
	}
	data := documentFixture(t, ".docx")
	for name, modify := range map[string]func([]byte){
		"forged-count": func(data []byte) {
			end := data[len(data)-22:]
			binary.LittleEndian.PutUint16(end[8:10], 1)
			binary.LittleEndian.PutUint16(end[10:12], 1)
		},
		"zip64": func(data []byte) {
			binary.LittleEndian.PutUint16(data[len(data)-22+10:], 0xffff)
		},
		"central-bound": func(data []byte) {
			binary.LittleEndian.PutUint32(data[len(data)-22+12:], 2<<20+1)
		},
	} {
		bad := bytes.Clone(data)
		modify(bad)
		if _, err := boundedDocumentZIP(bad); err == nil {
			t.Fatalf("invalid %s ZIP accepted", name)
		}
	}
	for length := 0; length < len(data); length++ {
		if _, err := boundedDocumentZIP(data[:length]); err == nil {
			t.Fatalf("truncated ZIP prefix of %d bytes accepted", length)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := validateReadDocument(ctx, data, documentCapability(".docx").MIMEType); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled container validation returned %v", err)
	}
	for extension, index := range map[string]string{".pages": "index.xml", ".key": "index.apxl.gz"} {
		data := zipFixture(t, map[string]string{index: "original bytes"})
		if err := validateReadDocument(context.Background(), data, documentCapability(extension).MIMEType); err != nil {
			t.Fatalf("legacy iWork ZIP index rejected: %v", err)
		}
	}
}
