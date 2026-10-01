package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchRejectsUnsupportedTextBeforeMutating(t *testing.T) {
	for _, action := range []string{"add", "update"} {
		t.Run(action, func(t *testing.T) {
			r, w, execution, request := toolFixture(t)
			path := filepath.Join(w.Root, "text.txt")
			patch := "*** Begin Patch\n*** Add File: text.txt\n+hello\x00world\n*** End Patch"
			if action == "update" {
				if err := os.WriteFile(path, []byte("original\n"), 0600); err != nil {
					t.Fatal(err)
				}
				patch = "*** Begin Patch\n*** Update File: text.txt\n@@\n-original\n+hello\x00world\n*** End Patch"
			}
			args, err := json.Marshal(map[string]string{"patch_text": patch})
			if err != nil {
				t.Fatal(err)
			}
			record := invoke(t, r, w, execution, request, "patch", string(args))
			if !strings.Contains(string(record.Result), `"ok":false`) || !strings.Contains(string(record.Result), "without NUL") {
				t.Fatalf("unsupported patch needs an actionable error: %s", record.Result)
			}
			data, err := os.ReadFile(path)
			if action == "add" {
				if !os.IsNotExist(err) {
					t.Fatalf("failed add created a file: %q, %v", data, err)
				}
			} else if err != nil || string(data) != "original\n" {
				t.Fatalf("failed update changed existing text: %q, %v", data, err)
			}
		})
	}
	if _, err := parsePatch("*** Begin Patch\n*** Add File: text.txt\n+invalid\xff\n*** End Patch"); err == nil {
		t.Fatal("invalid UTF-8 patch accepted")
	}
}
