package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/capture"
	"ttc/internal/jobs"
	"ttc/internal/prompts"
)

func TestRegisteredToolIntegerClipping(t *testing.T) {
	r, w, _, _ := toolFixture(t)
	AddShell(r, nil, w, nil)
	AddWeb(r, http.DefaultClient, WebSearchConfig{})
	for _, tc := range []struct {
		name, args, parameter string
		minimum, maximum      int
	}{
		{"read", `{"path":"text"}`, "limit", 1, 2000},
		{"glob", `{"pattern":"*"}`, "limit", 1, 500},
		{"grep", `{"pattern":"text"}`, "limit", 1, 500},
		{"shell", `{"command":"true"}`, "output_bytes", 1, capture.MaxOutputBytes},
		{"shell", `{"command":"true"}`, "timeout_seconds", 1, 86400},
		{"shell", `{"command":"true","background":true}`, "timeout_seconds", 1, 86400},
		{"job_read", `{"job_id":"missing"}`, "limit_bytes", 1, 65536},
		{"lsp_query", `{"job_id":"missing","operation":"document_symbols","path":"text"}`, "limit", 1, 500},
		{"lsp_query", `{"job_id":"missing","operation":"document_symbols","path":"text"}`, "timeout_seconds", 1, 120},
		{"web_fetch", `{"url":"https://example.test"}`, "max_chars", 1, 64000},
		{"web_fetch", `{"url":"https://example.test"}`, "timeout_seconds", 1, 120},
		{"web_fetch", `{"url":"https://example.test","pattern":"text"}`, "context_lines", 0, 10},
		{"web_search", `{"query":"text"}`, "num_results", 1, 10},
		{"web_search", `{"query":"text"}`, "max_chars", 1, 64000},
	} {
		t.Run(tc.name+"/"+tc.parameter+"/"+tc.args, func(t *testing.T) {
			codec, _ := r.Get(tc.name)
			for _, requested := range []int{-1, tc.minimum - 1, tc.minimum, tc.maximum, tc.maximum + 1} {
				args := strings.TrimSuffix(tc.args, "}") + fmt.Sprintf(",%q:%d}", tc.parameter, requested)
				call, err := codec.DecodeCall(1, []byte(args))
				if err != nil {
					t.Fatalf("%s: %v", args, err)
				}
				_, encoded, err := call.Encode()
				if err != nil {
					t.Fatal(err)
				}
				var effective map[string]json.RawMessage
				if err := json.Unmarshal(encoded, &effective); err != nil {
					t.Fatal(err)
				}
				want := max(tc.minimum, min(tc.maximum, requested))
				if tc.parameter == "timeout_seconds" && requested == 0 {
					want = tc.maximum
				}
				if string(effective[tc.parameter]) != fmt.Sprint(want) {
					t.Fatalf("%s: effective input %s", args, encoded)
				}
				warnings := call.InputWarnings()
				if requested == want {
					if len(warnings) != 0 {
						t.Fatalf("in-range input warned: %+v", warnings)
					}
					continue
				}
				if len(warnings) != 1 {
					t.Fatalf("expected one warning: %+v", warnings)
				}
				warning := warnings[0]
				if tc.parameter == "timeout_seconds" && requested == 0 {
					if warning.Code != "input_normalized" || warning.Parameter != tc.parameter || warning.Requested != 0 || warning.Effective != want {
						t.Fatalf("incorrect zero timeout warning: %+v", warning)
					}
					continue
				}
				if warning.Code != "input_clipped" || warning.Parameter != tc.parameter || warning.Requested != requested || warning.Effective != want || warning.Minimum != tc.minimum || warning.Maximum != tc.maximum || warning.Message != fmt.Sprintf(prompts.ToolInputClipped, tc.parameter, requested, want, tc.minimum, tc.maximum) {
					t.Fatalf("incorrect warning: %+v", warning)
				}
			}
		})
	}
}

func TestClippedFileAndShellExecution(t *testing.T) {
	r, w, x, req := toolFixture(t)
	if err := os.WriteFile(filepath.Join(w.Root, "text"), []byte("first\nsecond\n"), 0600); err != nil {
		t.Fatal(err)
	}
	record := invoke(t, r, w, x, req, "read", `{"path":"text","limit":0}`)
	ok(t, record)
	var page struct {
		Content  string
		Warnings []InputWarning
	}
	if err := json.Unmarshal(record.Result, &page); err != nil || page.Content != "first\n" || len(page.Warnings) != 1 {
		t.Fatalf("clipped read not executed: %s, %v", record.Result, err)
	}
	m := jobs.New(context.Background(), nil)
	defer m.Close()
	AddShell(r, m, w, nil)
	for _, requested := range []int{0, -1, capture.MaxOutputBytes + 1} {
		args := fmt.Sprintf(`{"command":"printf abc; exit 7","output_bytes":%d}`, requested)
		record := invoke(t, r, w, x, req, "shell", args)
		ok(t, record)
		var result struct {
			jobs.Snapshot
			Warnings []InputWarning
		}
		if err := json.Unmarshal(record.Result, &result); err != nil || result.ExitCode == nil || *result.ExitCode != 7 || len(result.Warnings) != 1 {
			t.Fatalf("clipped shell failed: %s, %v", record.Result, err)
		}
		want := "abc"
		if requested < 1 {
			want = "a"
		}
		if result.Stdout != want || string(record.Arguments) != args {
			t.Fatalf("incorrect execution/history: %+v", record)
		}
	}
}

func TestClippedDirectoryReadUsesDecodedFieldNames(t *testing.T) {
	r, w, x, req := toolFixture(t)
	if err := os.WriteFile(filepath.Join(w.Root, "entry"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{
		`{"path":".","Limit":-1}`,
		`{"path":".","limit":10,"LIMIT":-1}`,
	} {
		record := invoke(t, r, w, x, req, "read", args)
		ok(t, record)
		var result struct {
			Warnings []InputWarning
		}
		if err := json.Unmarshal(record.Result, &result); err != nil || len(result.Warnings) != 1 || result.Warnings[0].Requested != float64(-1) || result.Warnings[0].Effective != float64(1) || string(record.Arguments) != args {
			t.Fatalf("directory limit bypassed clipping: %s, %v", record.Result, err)
		}
	}
}
