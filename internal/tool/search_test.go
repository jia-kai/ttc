package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeSearchSyntaxPathsLimitsAndErrors(t *testing.T) {
	r, w, x, req := toolFixture(t)
	files := map[string]string{
		"-odd\nname.txt": "Alpha 12\nalpha 34\nliteral [bracket]\n",
		"second.txt":     "Alpha 56\nAlpha 78\n",
		"ignored.txt":    "Alpha 90\n",
		".gitignore":     "ignored.txt\n",
		"binary.dat":     "\x00Alpha 99\n",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(w.Root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	search := func(args string) struct {
		Matches   []searchMatch `json:"matches"`
		Truncated bool          `json:"truncated"`
	} {
		t.Helper()
		record := invoke(t, r, w, x, req, "grep", args)
		ok(t, record)
		var result struct {
			Matches   []searchMatch `json:"matches"`
			Truncated bool          `json:"truncated"`
		}
		if err := json.Unmarshal(record.Result, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	// Rust regex character-class intersection is unsupported by Go's regexp engine.
	result := search(`{"pattern":"[0-9&&[^02468]]"}`)
	if len(result.Matches) != 4 || result.Truncated {
		t.Fatal(result)
	}
	if result.Matches[0].Path != "-odd\nname.txt" || result.Matches[0].Line != 1 {
		t.Fatal("filename and line metadata must survive JSON framing", result)
	}
	if result := search(`{"pattern":"Alpha","case_sensitive":false,"limit":2}`); len(result.Matches) != 2 || !result.Truncated {
		t.Fatal("result limit must stop the native search", result)
	}
	if result := search(`{"pattern":"[bracket]","literal":true}`); len(result.Matches) != 1 || result.Matches[0].Text != "literal [bracket]" {
		t.Fatal(result)
	}
	args, err := json.Marshal(map[string]any{"path": "-odd\nname.txt", "pattern": "Alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if result := search(string(args)); len(result.Matches) != 1 {
		t.Fatal("explicit unusual file target failed", result)
	}
	if result := search(`{"pattern":"does-not-exist"}`); len(result.Matches) != 0 || result.Truncated {
		t.Fatal("rg exit 1 must mean an empty successful search", result)
	}
	for _, args := range []string{`{"pattern":"["}`, `{"pattern":"Alpha","include":"["}`} {
		record := invoke(t, r, w, x, req, "grep", args)
		if strings.Contains(string(record.Result), `"ok":true`) {
			t.Fatal("native regex/glob error must be reported", string(record.Result))
		}
	}
}

func TestSearchCancellationAndOversizeLineAreBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runSearch(ctx, t.TempDir(), []string{"--files"}, splitNull, func([]byte) (bool, error) { return false, nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled search failed to return its context error: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "long.txt"), []byte(strings.Repeat("match", 300000)), 0600); err != nil {
		t.Fatal(err)
	}
	truncated, err := runSearch(context.Background(), dir, []string{"--no-config", "--json", "--regexp", "match", "--", "."}, bufio.ScanLines, func([]byte) (bool, error) { return false, nil })
	if err != nil || !truncated {
		t.Fatal("oversized native output must truncate and reap the child", truncated, err)
	}
}
