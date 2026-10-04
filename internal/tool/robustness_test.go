package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
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
			page, err := readPage(context.Background(), path, 1, 1, nil)
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
			if _, err := readPage(context.Background(), path, 2, 1, nil); err == nil {
				t.Fatal("invalid second-page content unexpectedly succeeded")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "text")
	if err := os.WriteFile(path, []byte("ok\n"+strings.Repeat("x", 40001)), 0600); err != nil {
		t.Fatal(err)
	}
	page, err := readPage(context.Background(), path, 1, 200, nil)
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
	if _, err := readPage(ctx, dir, 1, 200, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled directory read returned %v", err)
	}
	for _, name := range []string{"z", "a", "m"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	page, err := readPage(context.Background(), dir, 2, 1, nil)
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
	if _, err := readPage(context.Background(), dir, 1, 1, nil); err != nil {
		t.Fatalf("directory at the limit rejected: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "overflow"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = readPage(context.Background(), dir, 1, 1, nil)
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

func TestReadUsesOpenedDescriptorWhenPathIsReplaced(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprint("directory=", directory), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "target")
			if directory {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "item"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte("original\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			// A path reopen would now block. Both branches must consume the
			// descriptor that was validated, irrespective of later replacements.
			page, err := readOpenedPage(context.Background(), f, path, 1, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(page)
			want := "original"
			if directory {
				want = "item"
			}
			if !strings.Contains(string(data), want) {
				t.Fatalf("read replacement instead of opened descriptor: %s", data)
			}
		})
	}
}

func TestReadSymlinksAndRejectsFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(path, []byte("linked\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPage(context.Background(), link, 1, 1, nil); err != nil {
		t.Fatalf("ordinary symlink reads must remain supported: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := readPage(ctx, link, 1, 1, nil)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "unsupported_content" {
		t.Fatalf("FIFO must be rejected from its descriptor: %v", err)
	}
}

func TestEditRejectsExpansionBeforeAllocation(t *testing.T) {
	r, w, x, req := toolFixture(t)
	path := filepath.Join(w.Root, "text")
	before := strings.Repeat("a", 4096)
	if err := os.WriteFile(path, []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal(map[string]any{"path": "text", "old_text": "a", "new_text": strings.Repeat("b", 16384), "replace_all": true})
	if err != nil {
		t.Fatal(err)
	}
	var start, end runtime.MemStats
	runtime.ReadMemStats(&start)
	record := invoke(t, r, w, x, req, "edit", string(args))
	runtime.ReadMemStats(&end)
	if !strings.Contains(string(record.Result), `"code":"file_too_large"`) || !strings.Contains(string(record.Result), "replace fewer occurrences") {
		t.Fatalf("oversized expansion needs actionable rejection: %s", record.Result)
	}
	// These small arguments describe a 64 MiB result. The file limit must
	// prevent allocation, rather than reject the result after constructing it.
	if allocated := end.TotalAlloc - start.TotalAlloc; allocated > 32<<20 {
		t.Fatalf("rejected expansion allocated %d bytes", allocated)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != before {
		t.Fatalf("rejected edit changed file: %q, %v", data, err)
	}
	ok(t, invoke(t, r, w, x, req, "edit", `{"path":"text","old_text":"a","new_text":"","replace_all":true}`))
	data, err = os.ReadFile(path)
	if err != nil || len(data) != 0 {
		t.Fatalf("shrinking replacements must still work: %q, %v", data, err)
	}
}
