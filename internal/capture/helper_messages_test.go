package capture

import (
	"testing"

	"ttc/internal/prompts"
)

func TestReadValidationUsesPromptAssets(t *testing.T) {
	b := NewPool(64).NewBuffer(32)
	b.Write([]byte("界"))
	for _, test := range []struct {
		cursor string
		limit  int
		want   string
	}{
		{"", 0, prompts.CapturePositiveReadLimit},
		{"eof:-1", 4, prompts.CaptureEOFCursorFormat},
		{"eof:1:bytes", 4, prompts.CaptureEOFOffset},
		{"eof:-1:words", 4, prompts.CaptureEOFUnit},
		{"-1", 4, prompts.CaptureAbsoluteCursor},
		{"4", 4, prompts.CaptureCursorBeyondOutput},
		{"0", 1, prompts.CaptureUTF8ReadLimit},
	} {
		_, err := b.Read(test.cursor, test.limit)
		if err == nil || err.Error() != test.want {
			t.Fatalf("Read(%q, %d) = %v, want %q", test.cursor, test.limit, err, test.want)
		}
	}
}
