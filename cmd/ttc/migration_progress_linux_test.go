package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"ttc/internal/history/migrate"
)

func TestMigrationProgressDoesNotRoundUpBeforeCompletion(t *testing.T) {
	progress := migrate.Progress{FromVersion: 6, ToVersion: 7, Completed: 100000, Total: 100002, Phase: "rebuild_entries"}
	line := migrationProgressLine(progress, 80)
	if strings.Contains(line, "100%") || !strings.Contains(line, "99% rebuild_entries") {
		t.Fatalf("unfinished rebuild looks complete: %q", line)
	}
	progress.Completed = progress.Total
	if line := migrationProgressLine(progress, 80); !strings.Contains(line, "100% rebuild_entries") {
		t.Fatalf("completed work did not reach 100%%: %q", line)
	}
}

func testMigrationProgress(terminal bool, width int) (*migrationProgress, *bytes.Buffer, *time.Time) {
	var out bytes.Buffer
	now := time.Unix(100, 0)
	return &migrationProgress{
		out: &out, terminal: terminal, width: func() int { return width },
		now: func() time.Time { return now },
	}, &out, &now
}

func TestMigrationProgressSeparateTransitions(t *testing.T) {
	p, out, _ := testMigrationProgress(true, 80)
	p.report(migrate.Progress{FromVersion: 5, ToVersion: 6, Total: 10, Phase: "scan"})
	p.report(migrate.Progress{FromVersion: 5, ToVersion: 6, Completed: 10, Total: 10, Phase: "scan"})
	p.report(migrate.Progress{FromVersion: 6, ToVersion: 7, Total: 20, Phase: "convert"})
	p.report(migrate.Progress{FromVersion: 6, ToVersion: 7, Completed: 20, Total: 20, Phase: "convert"})
	if strings.Contains(out.String(), "migration complete") {
		t.Fatal("claimed committed success before Open returned")
	}
	p.finish(nil)
	lines := strings.Split(out.String(), "\n")
	if len(lines) != 5 || !strings.Contains(lines[1], "History 5->6 [") || !strings.Contains(lines[2], "History 6->7 [") {
		t.Fatalf("transitions did not have separate bar lines: %q", out.String())
	}
	if !strings.Contains(lines[1], "100% scan") || !strings.Contains(lines[2], "100% convert") || lines[3] != "History migration complete." {
		t.Fatalf("missing phase or committed completion: %q", out.String())
	}
}

func TestMigrationProgressThrottlesExceptBoundaries(t *testing.T) {
	p, out, now := testMigrationProgress(true, 80)
	progress := migrate.Progress{FromVersion: 6, ToVersion: 7, Total: 100, Phase: "convert"}
	p.report(progress)
	initial := out.Len()
	progress.Completed = 10
	p.report(progress)
	if out.Len() != initial {
		t.Fatal("did not throttle a rapid update")
	}
	*now = now.Add(100 * time.Millisecond)
	p.report(progress)
	if out.Len() == initial {
		t.Fatal("did not render after throttle interval")
	}
	beforePhase := out.Len()
	progress.Phase = "copy table"
	p.report(progress)
	if out.Len() == beforePhase {
		t.Fatal("throttled a phase boundary")
	}
	beforeEnd := out.Len()
	progress.Completed = progress.Total
	p.report(progress)
	if out.Len() == beforeEnd {
		t.Fatal("throttled phase completion")
	}
}

func TestMigrationProgressErrorFinishesLine(t *testing.T) {
	p, out, _ := testMigrationProgress(true, 80)
	p.report(migrate.Progress{FromVersion: 6, ToVersion: 7, Completed: 10, Total: 10, Phase: "complete"})
	if !strings.Contains(out.String(), "complete (pending commit)") {
		t.Fatalf("completed step was not marked as uncommitted: %q", out.String())
	}
	p.finish(errors.New("failed to commit"))
	if !strings.HasSuffix(out.String(), "\nHistory migration failed.\n") || p.lineOpen {
		t.Fatalf("error left an unfinished progress line: %q", out.String())
	}
	if strings.Contains(out.String(), "History migration complete.") {
		t.Fatalf("error implied success: %q", out.String())
	}
}

func TestMigrationProgressSilentWithoutUpgrade(t *testing.T) {
	for _, err := range []error{nil, errors.New("open failed")} {
		p, out, _ := testMigrationProgress(true, 80)
		p.finish(err)
		if out.Len() != 0 {
			t.Fatalf("printed without a migration: %q", out.String())
		}
	}
}

func TestMigrationProgressRedirectedMilestones(t *testing.T) {
	p, out, now := testMigrationProgress(false, 80)
	progress := migrate.Progress{FromVersion: 6, ToVersion: 7, Total: 100, Phase: "convert"}
	p.report(progress)
	for _, completed := range []int64{10, 25, 30, 50, 60, 75, 80, 100} {
		*now = now.Add(100 * time.Millisecond)
		progress.Completed = completed
		p.report(progress)
	}
	p.finish(nil)
	got := out.String()
	if strings.ContainsAny(got, "\r\x1b") {
		t.Fatalf("redirected output contains terminal controls: %q", got)
	}
	if strings.Count(got, "History 6->7") != 5 {
		t.Fatalf("expected start and quarter milestones: %q", got)
	}
	for _, percent := range []string{"  0%", " 25%", " 50%", " 75%", "100%"} {
		if !strings.Contains(got, percent+" convert") {
			t.Fatalf("missing milestone %q: %q", percent, got)
		}
	}
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if newMigrationProgress(file).terminal {
		t.Fatal("regular file detected as terminal")
	}
}

func TestMigrationProgressLineFitsNarrowTerminal(t *testing.T) {
	progress := migrate.Progress{FromVersion: 5, ToVersion: 6, Completed: 5, Total: 10, Phase: "convert\x1b[2J\n\tdata"}
	for _, width := range []int{0, 1, 7, 12, 24, 40, 80} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			line := migrationProgressLine(progress, width)
			if len(line) > width || strings.ContainsAny(line, "\x1b\r\n\t") {
				t.Fatalf("unsafe line width %d: %q", width, line)
			}
			if width == 80 && (!strings.Contains(line, "[============------------]") || !strings.Contains(line, "50% convert")) {
				t.Fatalf("missing bar or phase: %q", line)
			}
		})
	}
}
