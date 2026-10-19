package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"ttc/internal/history"
	"ttc/internal/history/migrate"
)

func testStartupProgress(terminal bool, width int) (*startupProgress, *bytes.Buffer, *time.Time) {
	var out bytes.Buffer
	now := time.Unix(100, 0)
	return &startupProgress{
		out: &out, terminal: terminal, width: func() int { return width },
		now: func() time.Time { return now },
	}, &out, &now
}

func TestStartupProgressLine(t *testing.T) {
	for _, tc := range []struct {
		completed, total int64
		want             string
	}{
		{20, 0, "work ..."}, {-1, 10, "0% work (0/10)"},
		{5, 10, "50% work (5/10)"}, {100000, 100002, "99% work"},
		{1<<63 - 2, 1<<63 - 1, "99% work"}, {10, 10, "100% work"},
	} {
		p := progressState{"History", "work", tc.completed, tc.total}
		line := startupProgressLine(p, 80)
		if !strings.Contains(line, tc.want) || (tc.total == 0 && strings.ContainsAny(line, "%[]")) {
			t.Fatalf("%+v: %q, want %q", p, line, tc.want)
		}
	}
	for _, total := range []int64{0, 10, 1000000} {
		p := progressState{"History maintenance", "candidates (pending commit)", total / 2, total}
		for _, width := range []int{-1, 0, 1, 7, 24, 40, 79, 120} {
			line := startupProgressLine(p, width)
			if width >= 0 && len(line) > width {
				t.Fatalf("width %d: %q", width, line)
			}
			if width >= 79 && total > 0 && (!strings.Contains(line, "[=") || !strings.Contains(line, "pending commit")) {
				t.Fatalf("missing bar or pending label: %q", line)
			}
		}
	}
	p := progressState{"History\x1b[2J\r\n\t\x00\x7f\u009b\u202e界", "work\a\u2028", 5, 10}
	for _, r := range startupProgressLine(p, -1) {
		if r < ' ' || r > '~' {
			t.Fatalf("unsafe output character %U", r)
		}
	}
}

func TestStartupProgressUpdates(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			p, out, now := testStartupProgress(terminal, 80)
			for _, step := range []struct {
				completed int64
				phase     string
				advance   time.Duration
				print     bool
			}{
				{0, "work", 0, true}, {25, "work", 0, false},
				{25, "work", time.Second, true}, {30, "work", time.Second, terminal},
				{50, "work", time.Second, true}, {75, "work", time.Second, true},
				{100, "work", 0, true}, {100, "work", 0, false},
				{0, "commit", 0, true}, {20, "commit", time.Second, terminal},
			} {
				*now = now.Add(step.advance)
				before := out.Len()
				total := int64(100)
				if step.phase == "commit" {
					total = 0
				}
				p.report(progressState{"History", step.phase, step.completed, total})
				if (out.Len() > before) != step.print {
					t.Fatalf("step %+v: output %q", step, out.String()[before:])
				}
			}
			if !terminal && strings.ContainsAny(out.String(), "\r\x1b") {
				t.Fatalf("terminal controls in redirected output: %q", out.String())
			}
		})
	}
	for _, width := range []int{0, 1, 2, 7, 24, 40, 80} {
		p, out, now := testStartupProgress(true, width)
		p.report(progressState{"History", "a longer phase", 5, 10})
		*now = now.Add(time.Second)
		p.report(progressState{"History", "short", 6, 10})
		for _, frame := range strings.Split(out.String(), "\r") {
			if len(frame) > max(0, width-1) {
				t.Fatalf("autowrapping frame at width %d: %q", width, frame)
			}
		}
	}
}

func TestStartupProgressLifecycle(t *testing.T) {
	for _, migration := range []bool{false, true} {
		for _, failure := range []bool{false, true} {
			p, out, _ := testStartupProgress(true, 120)
			var err error
			status := "complete"
			if failure {
				err, status = errors.New("commit failed"), "failed"
			}
			p.finish("History", err)
			if out.Len() != 0 {
				t.Fatal("printed without work")
			}
			if migration {
				p.migration(migrate.Progress{FromVersion: 5, ToVersion: 6, Phase: "scan"})
				p.migration(migrate.Progress{FromVersion: 6, ToVersion: 7, Phase: "complete", Completed: 10, Total: 10})
				if !strings.Contains(out.String(), "\n\rHistory 6->7") {
					t.Fatalf("migration steps share a line: %q", out.String())
				}
			} else {
				for _, phase := range []string{"discovery", "indexes", "commit", "assets", "reclamation", "checkpoint", "vacuum", "statistics", "unknown"} {
					p.maintenance(history.MaintenanceProgress{Phase: phase})
				}
				if strings.ContainsAny(out.String(), "%[]") || !strings.Contains(out.String(), "unknown ...") {
					t.Fatalf("invented work totals or lost unknown phase: %q", out.String())
				}
				out.Reset()
				p.maintenance(history.MaintenanceProgress{Phase: "candidates", Completed: 4, Total: 4})
				if strings.Contains(out.String(), "deleted") {
					t.Fatal("tentative candidates claimed deletion", out.String())
				}
			}
			if !strings.Contains(out.String(), "pending commit") || strings.Contains(out.String(), "complete.") {
				t.Fatalf("premature success: %q", out.String())
			}
			p.finish("History", err)
			if !strings.HasSuffix(out.String(), "\nHistory "+status+".\n") || p.last.title != "" {
				t.Fatalf("unfinished or incorrect result: %q", out.String())
			}
		}
	}
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if newStartupProgress(file).terminal {
		t.Fatal("regular file detected as terminal")
	}
}
