package assets

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"ttc/internal/prompts"
)

func TestImageReadValidationUsesPromptAssets(t *testing.T) {
	if _, _, err := Read(t.TempDir()); err == nil || err.Error() != prompts.ImageNotRegularFile {
		t.Fatalf("nonregular image guidance = %v", err)
	}
	path := filepath.Join(t.TempDir(), "oversized.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxBytes + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Read(path); err == nil || err.Error() != fmt.Sprintf(prompts.ImageTooLarge, MaxBytes) {
		t.Fatalf("image size guidance = %v", err)
	}
}
