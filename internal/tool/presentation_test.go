package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/jobs"
	"ttc/internal/workspace"
)

func TestMutationViewsUseAppliedSnapshotDiffs(t *testing.T) {
	r, w, x, req := toolFixture(t)
	record := invoke(t, r, w, x, req, "write", `{"path":"sample.go","content":"package old\n"}`)
	ok(t, record)
	if !strings.Contains(record.Markdown.Summary, "+package old") || !strings.Contains(record.Markdown.Detail, "```diff") || strings.Contains(string(record.Result), "package old") {
		t.Fatal(record.Markdown, string(record.Result))
	}
	record = invoke(t, r, w, x, req, "edit", `{"path":"sample.go","old_text":"old","new_text":"new"}`)
	ok(t, record)
	for _, text := range []string{"-package old", "+package new", "**Old text:**", "**New text:**"} {
		if !strings.Contains(record.Markdown.Detail, text) {
			t.Fatal("missing", text, record.Markdown.Detail)
		}
	}
	if err := os.WriteFile(filepath.Join(w.Root, "sample.go"), []byte("external replacement\n"), 0600); err != nil {
		t.Fatal(err)
	}
	codec, _ := r.Get("edit")
	version, raw, err := record.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := codec.DecodeRecord(version, raw)
	if err != nil || decoded.Markdown.Detail != record.Markdown.Detail {
		t.Fatal(decoded, err)
	}
	patch := "*** Begin Patch\n*** Add File: created.txt\n+added\n*** Delete File: sample.go\n*** End Patch"
	args, _ := json.Marshal(map[string]string{"patch_text": patch})
	record = invoke(t, r, w, x, req, "patch", string(args))
	ok(t, record)
	if !strings.Contains(record.Markdown.Detail, "+added") || !strings.Contains(record.Markdown.Detail, "-external replacement") {
		t.Fatal(record.Markdown.Detail, string(record.Result))
	}
	record = invoke(t, r, w, x, req, "write", `{"path":"empty.txt","content":""}`)
	ok(t, record)
	if !strings.Contains(record.Markdown.Detail, "Created empty file") {
		t.Fatal(record.Markdown.Detail)
	}
	t.Setenv("PATH", t.TempDir())
	record = invoke(t, r, w, x, req, "write", `{"path":"without-diff.txt","content":"saved"}`)
	ok(t, record)
	if !strings.Contains(record.Markdown.Detail, "Diff unavailable") {
		t.Fatal(record.Markdown.Detail)
	}
}

func TestDiffCaptureIsBoundedAndIgnoresUnappliedPaths(t *testing.T) {
	dir := t.TempDir()
	blob := filepath.Join(dir, "snapshot")
	if err := os.WriteFile(blob, []byte(strings.Repeat("a long changed line for presentation\n", 20000)), 0600); err != nil {
		t.Fatal(err)
	}
	changes := []workspace.PathChange{
		{Path: "applied.txt", Applied: true, After: workspace.State{Exists: true, Blob: blob}},
		{Path: "not-applied.txt", Applied: false, After: workspace.State{Exists: true, Blob: blob}},
	}
	result := presentFiles(context.Background(), nil, changes)
	if !strings.Contains(result.detail, "Diff truncated") || len(result.detail) > 270000 || strings.Contains(result.detail, "not-applied") || strings.Count(result.summary, "\n") > 11 {
		t.Fatal("diff budget or applied-path filter", len(result.detail), result.summary)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result = presentFiles(ctx, nil, changes)
	if !strings.Contains(result.detail, "budget exhausted") {
		t.Fatal(result.detail)
	}
}

func TestShellStrictDefaultAndOptOut(t *testing.T) {
	r, w, x, req := toolFixture(t)
	m := jobs.New(context.Background(), nil)
	defer m.Close()
	AddShell(r, m, w, nil)
	for _, tc := range []struct {
		name, command, extra string
		wantExitZero         bool
	}{
		{"default", "false; printf should-not-run", "", false},
		{"explicit", "false; printf should-not-run", `,"strict":true`, false},
		{"unset", "unset TTC_TEST_UNSET_STRICT; printf '%s' \"$TTC_TEST_UNSET_STRICT\"; printf should-not-run", "", false},
		{"opt-out", "false; printf continued", `,"strict":false`, true},
		{"condition", "if false; then printf unused; fi; printf continued", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command, _ := json.Marshal(tc.command)
			record := invoke(t, r, w, x, req, "shell", `{"command":`+string(command)+tc.extra+`}`)
			ok(t, record)
			var view jobs.Snapshot
			if err := json.Unmarshal(record.Result, &view); err != nil || view.ExitCode == nil || (*view.ExitCode == 0) != tc.wantExitZero || strings.Contains(view.Stdout, "should-not-run") || view.Label != tc.command {
				t.Fatal(view, err)
			}
		})
	}
	record := invoke(t, r, w, x, req, "shell", `{"command":"false; printf should-not-run","background":true,"wake_on_exit":false}`)
	ok(t, record)
	var launched jobs.Snapshot
	json.Unmarshal(record.Result, &launched)
	view, err := m.Wait(context.Background(), "main", launched.ID, nil)
	if err != nil || view.ExitCode == nil || *view.ExitCode == 0 || view.Stdout != "" {
		t.Fatal(view, err)
	}
	record = invoke(t, r, w, x, req, "shell", `{"command":"true","strict":"false"}`)
	if !strings.Contains(string(record.Result), "invalid_input") {
		t.Fatal(string(record.Result))
	}
}
