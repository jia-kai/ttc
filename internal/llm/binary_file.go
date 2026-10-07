package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strings"
)

// UnmarshalJSON rejects obsolete inline binary inputs instead of silently
// discarding their bytes when decoding a native original reference.
func (f *BinaryFile) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if _, inline := fields["data_url"]; inline {
		return errors.New("inline binary data_url is unsupported; provide an original path and SHA-256 reference")
	}
	type reference BinaryFile
	var decoded reference
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*f = BinaryFile(decoded)
	return nil
}

// UnavailableBinaryFileError reports a valid reference whose original bytes are
// no longer cached and cannot be reconstructed. Adapters emit an explicit notice,
// preserving history and tool IDs. Invalid references and cache failures are fatal.
type UnavailableBinaryFileError struct {
	File BinaryFile
	Err  error // Failure to reconstruct the original bytes from File.Path.
}

// Error describes the omitted original and failed source verification.
func (e *UnavailableBinaryFileError) Error() string {
	return fmt.Sprintf("binary file unavailable: original bytes omitted for %q (expected SHA-256 %s); cache miss and source could not be verified: %v", e.File.Path, e.File.SHA256, e.Err)
}

// Unwrap returns the source reconstruction error.
func (e *UnavailableBinaryFileError) Unwrap() error { return e.Err }

// ValidBinaryMIME reports whether mt is a canonical MIME type without parameters,
// as required by native binary references and provider capability declarations.
func ValidBinaryMIME(mt string) bool {
	parsed, params, err := mime.ParseMediaType(mt)
	return err == nil && len(params) == 0 && parsed == mt && strings.Contains(mt, "/")
}
