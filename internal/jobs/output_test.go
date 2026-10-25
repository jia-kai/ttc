package jobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ttc/internal/capture"
)

func TestShellSelectedOutputMatchesCompletionNotification(t *testing.T) {
	for _, mode := range []string{"head", "tail", "head_tail"} {
		t.Run(mode, func(t *testing.T) {
			notified := make(chan Snapshot, 1)
			m := New(context.Background(), func(v Snapshot) { notified <- v })
			defer m.Close()
			id, err := m.Start("main", "printf 0123456789; printf abcdefghij >&2", t.TempDir(), time.Second, true, true, capture.OutputOptions{Mode: mode, Bytes: 8}, "")
			if err != nil {
				t.Fatal(err)
			}
			result, err := m.Wait(context.Background(), "main", id, nil)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case completion := <-notified:
				a, _ := json.Marshal(result)
				b, _ := json.Marshal(completion)
				if string(a) != string(b) {
					t.Fatalf("wait=%s notification=%s", a, b)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("missing completion")
			}
			if len(result.Stdout)+len(result.Stderr) > 8 || !result.Truncated || result.StdoutSelection.OmittedBytes != 6 || result.StderrSelection.OmittedBytes != 6 {
				t.Fatal(result)
			}
			page, err := m.Read(context.Background(), "main", id, ReadOptions{Limit: 64})
			if err != nil || page["output"] != "0123456789" {
				t.Fatal(page, err)
			}
		})
	}
}

func TestModestOutputIsCompleteIncludingBlankLines(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	id, err := m.Start("main", "i=0; while [ $i -lt 30 ]; do printf 'line-%s\\n' $i; i=$((i+1)); done; printf '\\n \\n'", t.TempDir(), time.Second, true, false, capture.OutputOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	result, err := m.Wait(context.Background(), "main", id, nil)
	if err != nil || !strings.HasPrefix(result.Stdout, "line-0\n") || !strings.HasSuffix(result.Stdout, "line-29\n\n \n") || result.Truncated {
		t.Fatal(result, err)
	}
}

func TestOutputSelectionLeavesCompactStreamingPreviews(t *testing.T) {
	// Streaming previews use the separate compact path, irrespective of budget.
	p := capture.NewPool(1 << 20)
	options := capture.OutputOptions{Bytes: capture.MaxOutputBytes}
	j := &job{stdout: p.NewBuffer(1 << 16), stderr: p.NewBuffer(1 << 16), output: &options}
	j.stdout.Write([]byte(strings.Repeat("x", 8000)))
	if got := compactPreview(j); len(got.Stdout) > 1024 {
		t.Fatal(len(got.Stdout))
	}
	if got := preview(j); len(got.Stdout) != 8000 || got.Truncated {
		t.Fatal(len(got.Stdout), got.Truncated)
	}
}

func TestStdoutTruncationDoesNotIncludeStderr(t *testing.T) {
	for _, selected := range []bool{false, true} {
		for _, largeStdout := range []bool{false, true} {
			p := capture.NewPool(1 << 20)
			j := &job{stdout: p.NewBuffer(1 << 16), stderr: p.NewBuffer(1 << 16)}
			if selected {
				j.output = &capture.OutputOptions{Bytes: 64}
			}
			stdout := "short"
			if largeStdout {
				stdout = strings.Repeat("x", 9000)
			}
			j.stdout.Write([]byte(stdout))
			j.stderr.Write([]byte(strings.Repeat("diagnostic", 900)))
			got := preview(j)
			if !got.Truncated || got.StdoutTruncated != largeStdout {
				t.Fatalf("selected=%t large_stdout=%t: %+v", selected, largeStdout, got)
			}
		}
	}
}
