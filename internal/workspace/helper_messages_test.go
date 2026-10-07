package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"ttc/internal/prompts"
)

func TestMutationRecoveryUsesPromptAssets(t *testing.T) {
	w, session, turn, request := fixture(t)
	path := filepath.Join(w.Root, "existing")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(w.Root, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := safePath(link); err == nil || err.Error() != fmt.Sprintf(prompts.WorkspaceSymlinkMutation, link) {
		t.Fatalf("symlink guidance = %v", err)
	}
	call := intent(t, w, session, turn, request)
	_, err := w.Apply(context.Background(), session.ID, call, []Mutation{{Path: path, MustAbsent: true, Data: []byte("replacement")}})
	if err == nil || err.Error() != fmt.Sprintf(prompts.WorkspaceTargetExists, path) {
		t.Fatalf("add destination guidance = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "original" {
		t.Fatalf("validation modified destination: %q, %v", data, err)
	}
}
