package openai_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"ttc/internal/binaryinput"
	"ttc/internal/blobcache"
	"ttc/internal/catalog"
	"ttc/internal/llm"
	"ttc/internal/providers/openai"
)

// TestBinaryLive is opt-in: six single-attempt subscription requests, one per
// format. These fixtures test text ingestion, not formatting or embedded images.
func TestBinaryLive(t *testing.T) {
	auth := os.Getenv("TTC_BINARY_LIVE_AUTH")
	if auth == "" {
		t.Skip("set TTC_BINARY_LIVE_AUTH to an explicit private subscription credential file")
	}
	a := openai.NewAdapter(openai.Config{TokenSource: openai.NewAuthenticator(auth).AccessTokens, ResolveBinary: binaryinput.Resolve})
	ctx, cancel := context.WithTimeout(context.Background(), 390*time.Second)
	defer cancel()
	manager, err := catalog.Open(ctx, catalog.Config{Bind: func(context.Context) (catalog.Binding, error) {
		return catalog.Binding{Scope: catalog.Scope{Provider: "openai", Endpoint: a.BaseURL, Version: openai.CatalogVersion}, Source: a, Validate: openai.ValidateCatalog}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	models := manager.Initial
	modelID := os.Getenv("TTC_BINARY_LIVE_MODEL")
	if modelID == "" {
		modelID = "gpt-5.6-luna"
	}
	selection, err := llm.Resolve("openai", models, modelID, "")
	if err != nil {
		t.Fatalf("choose a current standard model with TTC_BINARY_LIVE_MODEL: %v", err)
	}
	if selection.Model.ServiceTier != "" {
		t.Fatal("TTC_BINARY_LIVE_MODEL must select a standard-tier model")
	}
	for _, effort := range []string{"none", "minimal", "low"} {
		if slices.Contains(selection.Model.Variants, effort) {
			selection, err = llm.Resolve("openai", models, modelID, effort)
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cache, err := blobcache.Default()
	if err != nil {
		t.Fatal(err)
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	for _, format := range binaryLiveFormats() {
		t.Run(format.ext, func(t *testing.T) {
			callCtx, stop := context.WithTimeout(ctx, 60*time.Second)
			defer stop()
			marker := "TTC_" + hex.EncodeToString(nonce[:]) + "_" + strings.ToUpper(format.ext)
			data := format.generate(t, marker)
			advertised := false
			for _, capability := range selection.Model.BinaryFileTypes() {
				if capability.Kind == "document" && capability.MIMEType == format.mime && capability.MaxBytes >= len(data) {
					advertised = true
				}
			}
			if !advertised {
				t.Fatalf("model %s does not advertise %s", modelID, format.mime)
			}
			sum := sha256.Sum256(data)
			file := llm.BinaryFile{
				Path:   filepath.Join(t.TempDir(), "fixture."+format.ext),
				SHA256: hex.EncodeToString(sum[:]), MIMEType: format.mime, Bytes: len(data),
			}
			// Deliberately leave the source absent: success requires the original
			// cache entry, not inline data or a source-file fallback.
			if err := cache.Put(callCtx, "original", file.SHA256, data); err != nil {
				t.Fatal(err)
			}
			args, err := json.Marshal(map[string]string{"path": filepath.Base(file.Path)})
			if err != nil {
				t.Fatal(err)
			}
			request := llm.Request{
				ConversationID: "binary-live-" + hex.EncodeToString(nonce[:]) + "-" + format.ext,
				Selection:      selection, NoTools: true, MaxAttempts: 1, OutputTokens: 64,
				System: "Read the supplied file. Return only its exact marker text, or UNREADABLE if its contents cannot be accessed. Do not invoke tools.",
				Messages: []llm.Message{
					{Role: "user", Content: "Return the exact synthetic marker inside the attached file. Do not guess."},
					{Role: "assistant", Calls: []llm.ToolCall{{ID: "call_binary_live", Name: "read_file", Arguments: args}}},
					{Role: "tool", CallID: "call_binary_live", Content: "Attached original file.", Files: []llm.BinaryFile{file}},
				},
			}
			var output strings.Builder
			completed := false
			err = a.Stream(callCtx, request, func(event llm.StreamEvent) error {
				if event.Kind == "text" {
					output.WriteString(event.Text)
				}
				if event.Kind == "completed" {
					completed = true
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !completed || strings.TrimSpace(output.String()) != marker {
				t.Fatalf("completed=%t; output=%q; want marker %q", completed, output.String(), marker)
			}
			t.Logf("%s: %d original bytes, completed with exact marker (%s/%s)", format.ext, len(data), modelID, selection.Variant)
		})
	}
}

type binaryLiveFormat struct {
	ext, mime string
	generate  func(*testing.T, string) []byte
}

func binaryLiveFormats() []binaryLiveFormat {
	return []binaryLiveFormat{
		{"pdf", "application/pdf", binaryLivePDF},
		{"docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", binaryLiveDOCX},
		{"pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation", binaryLivePPTX},
		{"xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", binaryLiveXLSX},
		{"rtf", "application/rtf", func(_ *testing.T, marker string) []byte { return []byte(`{\rtf1\ansi ` + marker + `\par}`) }},
		{"odt", "application/vnd.oasis.opendocument.text", binaryLiveODT},
	}
}

func binaryLivePDF(_ *testing.T, marker string) []byte {
	stream := "BT /F1 12 Tf 36 72 Td (" + marker + ") Tj ET\n"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 144] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(stream), stream),
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, object := range objects {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return b.Bytes()
}

type binaryLiveZipEntry struct{ name, content string }

func binaryLiveZIP(t *testing.T, entries ...binaryLiveZipEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for _, entry := range entries {
		// Store these tiny entries with known sizes/CRC in the local header.
		// This also keeps ODF's first mimetype entry free of extra fields and descriptors.
		w, err := z.CreateRaw(&zip.FileHeader{
			Name: entry.name, Method: zip.Store,
			CRC32:            crc32.ChecksumIEEE([]byte(entry.content)),
			CompressedSize64: uint64(len(entry.content)), UncompressedSize64: uint64(len(entry.content)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, entry.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func binaryLiveOfficeZIP(t *testing.T, mainPath, mainType, mainXML string, entries ...binaryLiveZipEntry) []byte {
	t.Helper()
	contentTypes := `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/` + mainPath + `" ContentType="` + mainType + `"/>`
	for _, entry := range entries {
		if entry.name == "ppt/slides/slide1.xml" {
			contentTypes += `<Override PartName="/ppt/slides/slide1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>`
		}
		if entry.name == "xl/worksheets/sheet1.xml" {
			contentTypes += `<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`
		}
	}
	base := []binaryLiveZipEntry{
		{"[Content_Types].xml", contentTypes + `</Types>`},
		{"_rels/.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="` + mainPath + `"/></Relationships>`},
		{mainPath, mainXML},
	}
	return binaryLiveZIP(t, append(base, entries...)...)
}

func binaryLiveDOCX(t *testing.T, marker string) []byte {
	return binaryLiveOfficeZIP(t, "word/document.xml", "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml",
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>`+marker+`</w:t></w:r></w:p><w:sectPr/></w:body></w:document>`)
}

func binaryLivePPTX(t *testing.T, marker string) []byte {
	return binaryLiveOfficeZIP(t, "ppt/presentation.xml", "application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml",
		`<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst><p:sldId id="256" r:id="rId1"/></p:sldIdLst><p:sldSz cx="9144000" cy="6858000"/><p:notesSz cx="6858000" cy="9144000"/></p:presentation>`,
		binaryLiveZipEntry{"ppt/_rels/presentation.xml.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide1.xml"/></Relationships>`},
		binaryLiveZipEntry{"ppt/slides/slide1.xml", `<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><p:cSld><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/><p:sp><p:nvSpPr><p:cNvPr id="2" name="Text"/><p:cNvSpPr txBox="1"/><p:nvPr/></p:nvSpPr><p:spPr><a:xfrm><a:off x="360000" y="360000"/><a:ext cx="8000000" cy="1000000"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></p:spPr><p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:rPr sz="1800"/><a:t>` + marker + `</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`})
}

func binaryLiveXLSX(t *testing.T, marker string) []byte {
	return binaryLiveOfficeZIP(t, "xl/workbook.xml", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml",
		`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Sheet1" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		binaryLiveZipEntry{"xl/_rels/workbook.xml.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`},
		binaryLiveZipEntry{"xl/worksheets/sheet1.xml", `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>` + marker + `</t></is></c></row></sheetData></worksheet>`})
}

func binaryLiveODT(t *testing.T, marker string) []byte {
	return binaryLiveZIP(t,
		binaryLiveZipEntry{"mimetype", "application/vnd.oasis.opendocument.text"},
		binaryLiveZipEntry{"META-INF/manifest.xml", `<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0" manifest:version="1.2"><manifest:file-entry manifest:full-path="/" manifest:media-type="application/vnd.oasis.opendocument.text"/><manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"/></manifest:manifest>`},
		binaryLiveZipEntry{"content.xml", `<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" office:version="1.2"><office:body><office:text><text:p>` + marker + `</text:p></office:text></office:body></office:document-content>`})
}

// Validate generated ZIP/XML and marker isolation without credentials or network.
func TestBinaryLiveFixtures(t *testing.T) {
	for _, format := range binaryLiveFormats() {
		t.Run(format.ext, func(t *testing.T) {
			marker := "TTC_FIXTURE_" + strings.ToUpper(format.ext)
			data := format.generate(t, marker)
			count := bytes.Count(data, []byte(marker))
			if bytes.HasPrefix(data, []byte("PK")) {
				count = 0
				z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
				if err != nil {
					t.Fatal(err)
				}
				if format.ext == "odt" {
					first := z.File[0]
					if first.Name != "mimetype" || first.Method != zip.Store || first.Flags&8 != 0 || len(first.Extra) != 0 {
						t.Fatal("ODF mimetype must be first, stored, and have known local-header size")
					}
				}
				for _, entry := range z.File {
					r, err := entry.Open()
					if err != nil {
						t.Fatal(err)
					}
					content, err := io.ReadAll(r)
					if closeErr := r.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
					if err != nil {
						t.Fatal(err)
					}
					count += bytes.Count(content, []byte(marker))
					if strings.HasSuffix(entry.Name, ".xml") || strings.HasSuffix(entry.Name, ".rels") {
						decoder := xml.NewDecoder(bytes.NewReader(content))
						for {
							if _, err := decoder.Token(); err != nil {
								if err != io.EOF {
									t.Fatalf("%s: %v", entry.Name, err)
								}
								break
							}
						}
					}
				}
			}
			if count != 1 {
				t.Fatalf("marker occurs %d times; want one", count)
			}
		})
	}
}
