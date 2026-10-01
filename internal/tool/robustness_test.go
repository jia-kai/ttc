package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchMatchesCompleteLinesAndActualEOF(t *testing.T) {
	lines := []string{"@@", "-old", "+new"}
	for _, input := range []string{"prefix-old\n", "older\n", "older", "prefix-old"} {
		if got, err := updatePatch([]byte(input), lines); err == nil {
			t.Fatalf("partial-line input %q unexpectedly changed to %q", input, got)
		}
	}
	for _, input := range []string{"old\n", "old", "prefix-old\nold\n", "older\nold"} {
		got, err := updatePatch([]byte(input), lines)
		want := strings.TrimSuffix(strings.TrimSuffix(input, "\n"), "old") + "new"
		if strings.HasSuffix(input, "\n") {
			want += "\n"
		}
		if err != nil || string(got) != want {
			t.Fatalf("input %q: got %q, want %q, error %v", input, got, want, err)
		}
	}
	got, err := updatePatch([]byte("old\nold\n"), append(lines, "*** End of File"))
	if err != nil || string(got) != "old\nnew\n" {
		t.Fatalf("EOF marker must select final complete-line context: %q, %v", got, err)
	}
	if _, err := updatePatch(nil, []string{"@@", "-", "+new"}); err == nil {
		t.Fatal("missing-final-newline fallback accepted empty context")
	}
}

func TestGrepExplicitDashFile(t *testing.T) {
	r, w, x, req := toolFixture(t)
	if err := os.WriteFile(filepath.Join(w.Root, "-"), []byte("needle\n"), 0600); err != nil {
		t.Fatal(err)
	}
	record := invoke(t, r, w, x, req, "grep", `{"path":"-","pattern":"needle"}`)
	ok(t, record)
	var result struct{ Matches []searchMatch }
	if err := json.Unmarshal(record.Result, &result); err != nil || len(result.Matches) != 1 || result.Matches[0].Path != "-" {
		t.Fatalf("explicit dash filename interpreted as stdin: %s, %v", record.Result, err)
	}
}

func TestReadDoesNotValidateTheNextPage(t *testing.T) {
	for name, following := range map[string]string{"long": strings.Repeat("x", 40001), "invalid": "\xff\n"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "text")
			if err := os.WriteFile(path, []byte("ok\n"+following), 0600); err != nil {
				t.Fatal(err)
			}
			page, err := readPage(context.Background(), path, 1, 1)
			if err != nil {
				t.Fatalf("first page failed due to second-page content: %v", err)
			}
			b, _ := json.Marshal(page)
			var result struct {
				Content string
				Next    int `json:"next_offset"`
			}
			if err := json.Unmarshal(b, &result); err != nil || result.Content != "ok\n" || result.Next != 2 {
				t.Fatalf("wrong first page: %s, %v", b, err)
			}
			if _, err := readPage(context.Background(), path, 2, 1); err == nil {
				t.Fatal("invalid second-page content unexpectedly succeeded")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "text")
	if err := os.WriteFile(path, []byte("ok\n"+strings.Repeat("x", 40001)), 0600); err != nil {
		t.Fatal(err)
	}
	page, err := readPage(context.Background(), path, 1, 200)
	if err != nil {
		t.Fatalf("valid partial page discarded instead of returning next_offset: %v", err)
	}
	b, _ := json.Marshal(page)
	var result struct {
		Content string
		Next    int `json:"next_offset"`
	}
	if err := json.Unmarshal(b, &result); err != nil || result.Content != "ok\n" || result.Next != 2 {
		t.Fatalf("wrong partial page before oversized line: %s, %v", b, err)
	}
}

func TestDirectoryReadCancellationBoundsAndSortedPagination(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readPage(ctx, dir, 1, 200); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled directory read returned %v", err)
	}
	for _, name := range []string{"z", "a", "m"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	page, err := readPage(context.Background(), dir, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(page)
	var result struct {
		Entries []struct{ Name string }
		Next    int `json:"next_offset"`
	}
	if err := json.Unmarshal(b, &result); err != nil || len(result.Entries) != 1 || result.Entries[0].Name != "m" || result.Next != 3 {
		t.Fatalf("directory pagination is not sorted: %s, %v", b, err)
	}
	for i := 0; i < directoryEntryLimit-3; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("entry-%05d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := readDirectoryEntries(context.Background(), dir); err != nil {
		t.Fatalf("directory at the limit rejected: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "overflow"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = readPage(context.Background(), dir, 1, 1)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "directory_too_large" || !strings.Contains(failure.Message, "glob") {
		t.Fatalf("oversized directory must give bounded-search recovery guidance: %v", err)
	}
}

func TestRecoverableParameterErrorsIdentifyAdjustment(t *testing.T) {
	r, w, x, req := toolFixture(t)
	if err := os.WriteFile(filepath.Join(w.Root, "text"), []byte("same\nsame\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, args, guidance string }{
		{"read", `{"path":"text","limit":0}`, "limit must be 1"},
		{"edit", `{"path":"text","old_text":"missing","new_text":"new"}`, "read current contents"},
		{"edit", `{"path":"text","old_text":"same","new_text":"new"}`, "replace_all=true"},
	} {
		record := invoke(t, r, w, x, req, test.name, test.args)
		if !strings.Contains(string(record.Result), test.guidance) || strings.Contains(string(record.Result), `"ok":true`) {
			t.Fatalf("missing actionable guidance %q: %s", test.guidance, record.Result)
		}
	}
	b, err := os.ReadFile(filepath.Join(w.Root, "text"))
	if err != nil || string(b) != "same\nsame\n" {
		t.Fatalf("failed edits changed the file: %q, %v", b, err)
	}
}
