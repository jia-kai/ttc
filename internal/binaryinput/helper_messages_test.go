package binaryinput

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"testing"

	"ttc/internal/llm"
	"ttc/internal/prompts"
)

func TestBinaryReadValidationUsesPromptAssets(t *testing.T) {
	valid := llm.BinaryFileType{Kind: "document", MIMEType: "application/pdf", MaxBytes: 8}
	invalid := valid
	invalid.MaxBytes = 0
	for _, test := range []struct {
		name       string
		data       string
		size       int64
		types      []llm.BinaryFileType
		code, want string
	}{
		{"unannounced", "", 0, nil, "unsupported_binary_input", fmt.Sprintf(prompts.BinaryUnsupportedInput, "application/pdf")},
		{"capability", "", 0, []llm.BinaryFileType{invalid}, "invalid_binary_capability", prompts.BinaryInvalidCapability},
		{"oversized", "", 9, []llm.BinaryFileType{valid}, "binary_too_large", fmt.Sprintf(prompts.BinaryTooLarge, "8 bytes")},
		{"empty", "", 0, []llm.BinaryFileType{valid}, "unsupported_content", prompts.BinaryEmpty},
		{"invalid document", "bad", 3, []llm.BinaryFileType{valid}, "unsupported_content", fmt.Sprintf(prompts.BinaryInvalidDocument, "application/pdf", errors.New("missing PDF version header"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := readBinary(context.Background(), bytes.NewBufferString(test.data), test.size, "document", "application/pdf", ".pdf", test.types)
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != test.code || failure.Message != test.want {
				t.Fatalf("validation = %v, want %s: %q", err, test.code, test.want)
			}
		})
	}
}

func TestImageHeaderGuidancePreservesWrappedDecoderError(t *testing.T) {
	_, _, err := ImageConfig([]byte("not an image"))
	if !errors.Is(err, image.ErrFormat) || err.Error() != fmt.Errorf(prompts.ImageHeaderDecode, image.ErrFormat).Error() {
		t.Fatalf("header validation lost its guidance or wrapped cause: %v", err)
	}
}
