package jobs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ttc/internal/capture"
)

func TestShellCleanViewsAndRawReads(t *testing.T) {
	for _, rawView := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "raw"}[rawView], func(t *testing.T) {
			notified := make(chan Snapshot, 1)
			m := New(context.Background(), func(s Snapshot) { notified <- s })
			defer m.Close()
			strip := !rawView
			id, err := m.Start("main", "printf '\\033[31m10%%\\r100%%\\033[K\\033[0m\\n'; printf '\\033[32merror\\033[0m\\n' >&2", t.TempDir(), time.Second, true, true, true, capture.OutputOptions{StripANSI: &strip}, "")
			if err != nil {
				t.Fatal(err)
			}
			result, err := m.Wait(context.Background(), "main", id, nil)
			if err != nil {
				t.Fatal(err)
			}
			if rawView {
				if !strings.Contains(result.Stdout, "\x1b[31m") {
					t.Fatal(result)
				}
			} else if result.Stdout != "100%\n" || result.Stderr != "error\n" {
				t.Fatal(result)
			}
			select {
			case complete := <-notified:
				if complete.Stdout != result.Stdout || complete.Stderr != result.Stderr {
					t.Fatal(complete, result)
				}
			case <-time.After(time.Second):
				t.Fatal("missing notification")
			}
			clean, err := m.Read(context.Background(), "main", id, ReadOptions{Limit: 1024})
			if err != nil || clean["output"] != "100%\n" {
				t.Fatal(clean, err)
			}
			raw := false
			original, err := m.Read(context.Background(), "main", id, ReadOptions{Limit: 1024, StripANSI: &raw})
			if err != nil || !strings.Contains(original["output"].(string), "\x1b[31m") {
				t.Fatal(original, err)
			}
			m.mu.Lock()
			progress := compactPreview(m.jobs[id])
			m.mu.Unlock()
			if progress.Stdout != result.Stdout || progress.Stderr != result.Stderr {
				t.Fatal(progress, result)
			}
		})
	}
}

func TestCleanGrepRawOriginsAndInspection(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	raw := "\x1b[31mfirst\x1b[0m\n\x1b[32msecond\x1b[0m\n"
	id, err := m.StartTask("main", TaskOptions{Kind: "watch"}, func(ctx context.Context, out, stderr io.Writer) (TaskResult, error) {
		_, err := io.WriteString(out, raw)
		return TaskResult{}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Wait(context.Background(), "main", id, nil); err != nil {
		t.Fatal(err)
	}
	page, err := m.Read(context.Background(), "main", id, ReadOptions{Limit: 1024, Grep: "second"})
	if err != nil || page["output"] != "second\n" {
		t.Fatal(page, err)
	}
	origin := page["grep_origin"].(GrepOrigin)
	if origin.MatchCount != 1 || origin.EndByte != int64(len(raw)) {
		t.Fatal(origin)
	}
	inspection, err := m.Inspect(context.Background(), "main", id)
	if err != nil || inspection.Output[0].Text != "first\nsecond\n" {
		t.Fatal(inspection, err)
	}
}

func TestWatchCleanPresentationLeavesRecordRaw(t *testing.T) {
	dir, _ := watchFixture(t, true)
	raw := "\x1b[31m10%\r100%\x1b[K\x1b[0m\n"
	path := filepath.Join(dir, "stdout")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	m := New(context.Background(), nil)
	defer m.Close()
	id, err := m.StartWatch("main", dir, false, "", capture.OutputOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := m.Wait(ctx, "main", id, nil)
	if err != nil || result.Stdout != "100%\n" {
		t.Fatal(result, err)
	}
	bytes, err := os.ReadFile(path)
	if err != nil || string(bytes) != raw {
		t.Fatal(string(bytes), err)
	}
	inspection, err := m.Inspect(ctx, "main", id)
	if err != nil || inspection.Output[0].Text != "100%\n" {
		t.Fatal(inspection, err)
	}
	strip := false
	rawID, err := m.StartWatch("main", dir, false, "", capture.OutputOptions{StripANSI: &strip})
	if err != nil {
		t.Fatal(err)
	}
	rawResult, err := m.Wait(ctx, "main", rawID, nil)
	if err != nil || rawResult.Stdout != raw {
		t.Fatal(rawResult, err)
	}
	rawInspection, err := m.Inspect(ctx, "main", rawID)
	if err != nil || rawInspection.Output[0].Text != raw {
		t.Fatal(rawInspection, err)
	}
}
