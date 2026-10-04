package tool

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"ttc/internal/assets"
	"ttc/internal/provider"
)

// binaryKind preserves content-based image detection, then classifies documents
// using the producing request's catalog. Text MIME types retain pagination.
// Recognizable unannounced containers never fall through to ASCII text reads.
func binaryKind(path string, header []byte, types []provider.BinaryFileType) (kind, mime, extension string) {
	if mime = imageMIME(header); mime != "" {
		return "image", mime, ""
	}
	extension = strings.ToLower(filepath.Ext(path))
	for _, format := range types {
		if format.Kind != "document" || paginatedTextMIME(format.MIMEType) {
			continue
		}
		for _, ext := range format.Extensions {
			if normalizedExtension(ext) == extension && extension != "" {
				return "document", "", extension
			}
		}
	}
	switch {
	case bytes.HasPrefix(header, []byte("%PDF-")):
		return "document", "application/pdf", ".pdf"
	case bytes.HasPrefix(header, []byte("{\\rtf")):
		return "document", "application/rtf", ".rtf"
	case bytes.HasPrefix(header, []byte("PK\x03\x04")), bytes.HasPrefix(header, []byte("PK\x05\x06")):
		return "document", "application/zip", ""
	case bytes.HasPrefix(header, oleMagic):
		return "document", "application/x-ole-storage", ""
	default:
		return "", "", ""
	}
}

func normalizedExtension(extension string) string {
	return "." + strings.ToLower(strings.TrimPrefix(extension, "."))
}

func canonicalMIME(mime string) string {
	mime, _, _ = strings.Cut(mime, ";")
	mime = strings.ToLower(strings.TrimSpace(mime))
	if mime == "text/rtf" {
		return "application/rtf"
	}
	return mime
}

func paginatedTextMIME(mime string) bool {
	mime = canonicalMIME(mime)
	if strings.HasPrefix(mime, "text/") || strings.HasSuffix(mime, "+json") || strings.HasSuffix(mime, "+xml") {
		return true
	}
	switch mime {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml", "application/javascript", "application/x-sh", "application/sql", "application/toml":
		return true
	default:
		return false
	}
}

func readBinary(ctx context.Context, reader io.Reader, size int64, kind, mime, extension string, types []provider.BinaryFileType) (any, error) {
	var capability *provider.BinaryFileType
	for i := range types {
		t := &types[i]
		if t.Kind != kind || paginatedTextMIME(t.MIMEType) {
			continue
		}
		if mime != "" && canonicalMIME(t.MIMEType) == canonicalMIME(mime) {
			capability = t
			break
		}
		if kind == "document" && mime == "" {
			for _, ext := range t.Extensions {
				if normalizedExtension(ext) == extension {
					capability = t
					break
				}
			}
		}
		if capability != nil {
			break
		}
	}
	if capability == nil {
		format := mime
		if format == "" {
			format = extension
		}
		return nil, Fail("unsupported_binary_input", fmt.Sprintf("selected model does not announce %s input; select a model supporting this format, or use shell for explicit inspection/conversion", format))
	}
	if capability.MaxBytes <= 0 || strings.TrimSpace(capability.MIMEType) == "" {
		return nil, Fail("invalid_binary_capability", "provider binary capability must declare a MIME type and positive MaxBytes; fix the provider's model metadata")
	}
	maxBytes := min(assets.MaxBytes, capability.MaxBytes)
	tooLarge := func() error {
		bound := fmt.Sprintf("%d bytes", maxBytes)
		if maxBytes == assets.MaxBytes {
			bound = "32 MiB"
		}
		return Fail("binary_too_large", "binary file exceeds "+bound+"; provide a smaller file")
	}
	if size > int64(maxBytes) {
		return nil, tooLarge()
	}
	data, err := io.ReadAll(io.LimitReader(reader, int64(maxBytes)+1))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	// The descriptor may grow after Stat; enforce the same bound on actual bytes.
	if len(data) > maxBytes {
		return nil, tooLarge()
	}
	result := binaryRead{data: data, mime: capability.MIMEType, kind: kind}
	if kind == "image" {
		config, err := validateReadImage(data)
		if err != nil {
			return nil, Fail("unsupported_content", err.Error())
		}
		result.width, result.height = config.Width, config.Height
	} else if err := validateReadDocument(ctx, data, capability.MIMEType); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, Fail("unsupported_content", fmt.Sprintf("invalid %s document: %v; provide an original file matching its extension and announced MIME type", capability.MIMEType, err))
	}
	return result, ctx.Err()
}

var oleMagic = []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}

// validateReadDocument checks format/container identity, not document semantics.
// No document content is extracted, rendered, or converted. ZIP metadata is
// bounded before archive/zip allocates its directory, and only ODT's tiny MIME
// identifier is inflated with a hard limit. Unknown provider-announced MIME types
// pass through without local parsing: the catalog, not this validator, determines
// which original-byte formats can be submitted to the provider.
func validateReadDocument(ctx context.Context, data []byte, mime string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	mime = canonicalMIME(mime)
	switch mime {
	case "application/pdf":
		if len(data) < 8 || !bytes.HasPrefix(data, []byte("%PDF-")) || (data[5] != '1' && data[5] != '2') || data[6] != '.' || data[7] < '0' || data[7] > '9' {
			return fmt.Errorf("missing PDF version header")
		}
		if !bytes.Contains(data[max(0, len(data)-1024):], []byte("%%EOF")) {
			return fmt.Errorf("missing PDF end marker")
		}
		return nil
	case "application/rtf":
		if !bytes.HasPrefix(data, []byte("{\\rtf")) || len(data) < 7 || data[5] < '0' || data[5] > '9' || !bytes.HasSuffix(bytes.TrimSpace(data), []byte("}")) {
			return fmt.Errorf("missing RTF header or closing brace")
		}
		return nil
	case "application/msword", "application/vnd.ms-powerpoint", "application/vnd.ms-excel":
		if len(data) < 512 || !bytes.HasPrefix(data, oleMagic) || data[28] != 0xfe || data[29] != 0xff {
			return fmt.Errorf("missing OLE compound-file header")
		}
		version, shift := binary.LittleEndian.Uint16(data[26:28]), binary.LittleEndian.Uint16(data[30:32])
		if (version != 3 || shift != 9) && (version != 4 || shift != 12) {
			return fmt.Errorf("invalid OLE version/sector size")
		}
		if len(data) < 1<<shift || len(data)%(1<<shift) != 0 {
			return fmt.Errorf("incomplete OLE sector")
		}
		return nil
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "application/vnd.openxmlformats-officedocument.presentationml.presentation", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "application/vnd.oasis.opendocument.text", "application/vnd.apple.pages", "application/vnd.apple.keynote", "application/vnd.apple.iwork":
		archive, err := boundedDocumentZIP(data)
		if err != nil {
			return err
		}
		entries := make(map[string]*zip.File, len(archive.File))
		for _, file := range archive.File {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, exists := entries[file.Name]; exists {
				return fmt.Errorf("duplicate ZIP entry %q", file.Name)
			}
			entries[file.Name] = file
		}
		required := []string{}
		switch mime {
		case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
			required = []string{"[Content_Types].xml", "word/document.xml"}
		case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
			required = []string{"[Content_Types].xml", "ppt/presentation.xml"}
		case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
			required = []string{"[Content_Types].xml", "xl/workbook.xml"}
		case "application/vnd.oasis.opendocument.text":
			required = []string{"mimetype", "content.xml", "META-INF/manifest.xml"}
		case "application/vnd.apple.pages", "application/vnd.apple.keynote", "application/vnd.apple.iwork":
			// Modern iWork files share the IWA index. Older ZIP packages use
			// XML/APXL indexes; no index contents need to be decompressed.
			indexes := []string{"Index/Document.iwa", "index.xml", "index.xml.gz"}
			if mime == "application/vnd.apple.keynote" {
				indexes = []string{"Index/Document.iwa", "index.apxl", "index.apxl.gz"}
			} else if mime == "application/vnd.apple.iwork" {
				indexes = append(indexes, "index.apxl", "index.apxl.gz")
			}
			found := false
			for _, name := range indexes {
				if file := entries[name]; file != nil && !file.FileInfo().IsDir() {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("missing iWork document index")
			}
		}
		for _, name := range required {
			if entries[name] == nil || entries[name].FileInfo().IsDir() {
				return fmt.Errorf("missing ZIP entry %q", name)
			}
		}
		if mime == "application/vnd.oasis.opendocument.text" {
			file := entries["mimetype"]
			if file.UncompressedSize64 > 128 {
				return fmt.Errorf("oversized ODT MIME identifier")
			}
			r, err := file.Open()
			if err != nil {
				return err
			}
			mime, err := io.ReadAll(io.LimitReader(r, 129))
			closeErr := r.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			if string(mime) != "application/vnd.oasis.opendocument.text" {
				return fmt.Errorf("incorrect ODT MIME identifier")
			}
		}
		return ctx.Err()
	default:
		return ctx.Err()
	}
}

func boundedDocumentZIP(data []byte) (*zip.Reader, error) {
	if !bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		return nil, fmt.Errorf("missing ZIP local-file header")
	}
	// Find the real EOCD by its exact comment length, rather than trusting a
	// signature embedded in the archive comment. ZIP64 and multi-disk archives
	// are unnecessary for bounded documents and are rejected explicitly.
	for offset := len(data) - 22; offset >= max(0, len(data)-22-65535); offset-- {
		end := data[offset:]
		if !bytes.HasPrefix(end, []byte("PK\x05\x06")) || len(end) != 22+int(binary.LittleEndian.Uint16(end[20:22])) {
			continue
		}
		count := binary.LittleEndian.Uint16(end[10:12])
		centralBytes := binary.LittleEndian.Uint32(end[12:16])
		centralOffset := binary.LittleEndian.Uint32(end[16:20])
		if binary.LittleEndian.Uint16(end[4:6]) != 0 || binary.LittleEndian.Uint16(end[6:8]) != 0 || binary.LittleEndian.Uint16(end[8:10]) != count || count > 4096 || centralBytes > 2<<20 || uint64(centralOffset)+uint64(centralBytes) != uint64(offset) {
			return nil, fmt.Errorf("ZIP directory exceeds 4096 entries/2 MiB, uses ZIP64, or is invalid")
		}
		// Count real directory records before allocation: a forged EOCD count
		// must not let archive/zip allocate more than our declared entry cap.
		directory := data[int(centralOffset):offset]
		actual := 0
		for len(directory) > 0 {
			if len(directory) < 46 || !bytes.HasPrefix(directory, []byte("PK\x01\x02")) {
				return nil, fmt.Errorf("invalid ZIP directory record")
			}
			length := 46 + int(binary.LittleEndian.Uint16(directory[28:30])) + int(binary.LittleEndian.Uint16(directory[30:32])) + int(binary.LittleEndian.Uint16(directory[32:34]))
			actual++
			if length > len(directory) || actual > int(count) {
				return nil, fmt.Errorf("ZIP directory entry count or size mismatch")
			}
			directory = directory[length:]
		}
		if actual != int(count) {
			return nil, fmt.Errorf("ZIP directory entry count mismatch")
		}
		archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		if len(archive.File) != int(count) {
			return nil, fmt.Errorf("ZIP directory entry count mismatch")
		}
		return archive, nil
	}
	return nil, fmt.Errorf("missing ZIP end-of-directory record")
}
