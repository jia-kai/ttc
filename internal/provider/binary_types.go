package provider

import (
	"strings"

	"ttc/internal/blobcache"
)

// BinaryFileType announces a native attachment format and its original-byte limit.
// Extensions include the leading dot. Kind is "image" or "document".
type BinaryFileType struct {
	MIMEType   string   `json:"mime_type"`
	Extensions []string `json:"extensions"`
	Kind       string   `json:"kind"`
	MaxBytes   int      `json:"max_bytes"`
}

// BinaryFileTypes returns vision-gated image formats followed by declared
// document formats. The returned slice and extension slices are independently owned.
func (m ModelSpec) BinaryFileTypes() []BinaryFileType {
	out := []BinaryFileType{}
	if m.Images {
		out = []BinaryFileType{
			{MIMEType: "image/png", Extensions: []string{".png"}, Kind: "image", MaxBytes: blobcache.MaxBytes},
			{MIMEType: "image/jpeg", Extensions: []string{".jpg", ".jpeg"}, Kind: "image", MaxBytes: blobcache.MaxBytes},
			{MIMEType: "image/gif", Extensions: []string{".gif"}, Kind: "image", MaxBytes: blobcache.MaxBytes},
		}
	}
	for _, format := range m.BinaryFiles {
		format.Extensions = append([]string(nil), format.Extensions...)
		out = append(out, format)
	}
	return out
}

// EstimatedTokens reserves 4096 tokens for an image and at least that much for
// documents, using one token per original byte as a conservative expansion
// heuristic. This is not a bound: compressed text, PDF pages and provider-side
// document augmentation may consume more tokens than the original byte count.
func (f BinaryFile) EstimatedTokens() int {
	mimeType := f.MIMEType
	n := f.Bytes
	if f.DataURL != "" {
		if header, data, ok := strings.Cut(f.DataURL, ","); ok {
			mimeType = strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
			if n == 0 {
				n = len(data)/4*3 - (len(data) - len(strings.TrimRight(data, "=")))
			}
		}
	}
	if mimeType == "" || isImageMIME(mimeType) {
		return 4096
	}
	return max(4096, n)
}

func isImageMIME(mimeType string) bool {
	return mimeType == "image/png" || mimeType == "image/jpeg" || mimeType == "image/gif"
}
