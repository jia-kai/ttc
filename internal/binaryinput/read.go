// Package binaryinput classifies, validates and caches native binary originals
// using the producing request's provider capability catalog.
package binaryinput

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"ttc/internal/blobcache"
	"ttc/internal/llm"
)

// Error describes a binary input failure independently of a tool envelope.
type Error struct {
	Code    string // Stable failure category, such as unsupported_binary_input.
	Message string
}

// Error returns the failure category and actionable message.
func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Result holds a validated native original. Data owns the bytes read from the
// descriptor; callers must not modify it or the metadata before calling Store.
// Width and Height are the image canvas dimensions in pixels, or zero for documents.
type Result struct {
	Kind          string // "image" or "document".
	MIMEType      string // The selected capability's original MIME type.
	Width, Height int
	Data          []byte
}

// Store hashes and caches the exact original bytes, without reopening path.
// The returned reference labels those bytes with path, which need not still exist.
func (r *Result) Store(ctx context.Context, path string) (llm.BinaryFile, error) {
	if err := ctx.Err(); err != nil {
		return llm.BinaryFile{}, err
	}
	hash := sha256.Sum256(r.Data)
	checksum := hex.EncodeToString(hash[:])
	cache, err := blobcache.Default()
	if err != nil {
		return llm.BinaryFile{}, fmt.Errorf("open binary cache: %w", err)
	}
	if err := cache.Put(ctx, "original", checksum, r.Data); err != nil {
		return llm.BinaryFile{}, fmt.Errorf("cache binary file: %w", err)
	}
	return llm.BinaryFile{Path: path, SHA256: checksum, MIMEType: r.MIMEType, Bytes: len(r.Data)}, nil
}

// Read sniffs the caller's already-opened regular file without reopening path.
// size is its stat size in bytes; the actual read also enforces the smaller of
// the selected capability's limit and 32 MiB. Ordinary text returns (nil, nil)
// without consuming bytes, leaving reader available for text pagination.
// The caller owns the descriptor and must arrange to unblock reads on cancellation.
func Read(ctx context.Context, reader *bufio.Reader, path string, size int64, types []llm.BinaryFileType) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	header, err := reader.Peek(8)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil && err != io.EOF {
		return nil, err
	}
	kind, mime, extension := binaryKind(path, header, types)
	if kind == "" {
		return nil, nil
	}
	return readBinary(ctx, reader, size, kind, mime, extension, types)
}

func imageMIME(header []byte) string {
	switch {
	case bytes.HasPrefix(header, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(header, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(header, []byte("GIF87a")), bytes.HasPrefix(header, []byte("GIF89a")):
		return "image/gif"
	default:
		return ""
	}
}

// binaryKind preserves content-based image detection, then classifies documents
// using the producing request's catalog. Text MIME types retain pagination.
// Recognizable unannounced containers never fall through to ASCII text reads.
func binaryKind(path string, header []byte, types []llm.BinaryFileType) (kind, mime, extension string) {
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

func readBinary(ctx context.Context, reader io.Reader, size int64, kind, mime, extension string, types []llm.BinaryFileType) (*Result, error) {
	var capability *llm.BinaryFileType
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
		return nil, &Error{Code: "unsupported_binary_input", Message: fmt.Sprintf("selected model does not announce %s input; select a model supporting this format, or use shell for explicit inspection/conversion", format)}
	}
	if capability.MaxBytes <= 0 || !llm.ValidBinaryMIME(capability.MIMEType) {
		return nil, &Error{Code: "invalid_binary_capability", Message: "provider binary capability must declare a canonical MIME type without parameters and positive MaxBytes; fix the provider's model metadata"}
	}
	maxBytes := min(blobcache.MaxBytes, capability.MaxBytes)
	tooLarge := func() error {
		bound := fmt.Sprintf("%d bytes", maxBytes)
		if maxBytes == blobcache.MaxBytes {
			bound = "32 MiB"
		}
		return &Error{Code: "binary_too_large", Message: "binary file exceeds " + bound + "; provide a smaller file"}
	}
	if size > int64(maxBytes) {
		return nil, tooLarge()
	}
	data, overflow, err := readBinaryBytes(ctx, reader, size, maxBytes)
	if err != nil {
		return nil, err
	}
	// The descriptor may grow after Stat; enforce the same bound on actual bytes.
	if overflow {
		return nil, tooLarge()
	}
	if len(data) == 0 {
		return nil, &Error{Code: "unsupported_content", Message: "binary file is empty; provide a nonempty original file"}
	}
	result := &Result{Data: data, MIMEType: capability.MIMEType, Kind: kind}
	if kind == "image" {
		config, err := validateReadImage(ctx, data)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &Error{Code: "unsupported_content", Message: err.Error()}
		}
		result.Width, result.Height = config.Width, config.Height
	} else if err := validateReadDocument(ctx, data, capability.MIMEType); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Code: "unsupported_content", Message: fmt.Sprintf("invalid %s document: %v; provide an original file matching its extension and announced MIME type", capability.MIMEType, err)}
	}
	return result, ctx.Err()
}

// readBinaryBytes preallocates the stat size and reads in bounded chunks. Growth
// after Stat uses bounded geometric allocation; a full buffer probes one extra
// byte separately so unchanged files at the limit need only one large allocation.
// As with Read, the owner must unblock a descriptor read on cancellation.
func readBinaryBytes(ctx context.Context, reader io.Reader, size int64, limit int) ([]byte, bool, error) {
	data := make([]byte, 0, int(min(max(size, 0), int64(limit))))
	var extra [1]byte
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		var target []byte
		if len(data) == limit {
			target = extra[:]
		} else {
			if len(data) == cap(data) {
				grown := make([]byte, len(data), min(limit, max(binaryReadChunk, 2*cap(data))))
				copy(grown, data)
				data = grown
			}
			target = data[len(data):min(cap(data), len(data)+binaryReadChunk)]
		}
		n, err := reader.Read(target)
		if canceled := ctx.Err(); canceled != nil {
			return nil, false, canceled
		}
		if err != nil && err != io.EOF {
			return nil, false, err
		}
		if len(data) == limit && n > 0 {
			return nil, true, nil
		}
		data = data[:len(data)+n]
		if err == io.EOF {
			return data, false, nil
		}
		if n == 0 {
			emptyReads++
			if emptyReads == 100 {
				return nil, false, io.ErrNoProgress
			}
		} else {
			emptyReads = 0
		}
	}
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
