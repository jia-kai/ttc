package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"ttc/internal/history/migrate"

	"golang.org/x/term"
)

// migrationProgress owns only startup stderr output, before the TUI takes over.
// A full bar describes upgrade work, not a committed database transaction.
type migrationProgress struct {
	out        io.Writer
	terminal   bool
	width      func() int
	now        func() time.Time
	last       migrate.Progress
	lastDraw   time.Time
	started    bool
	lineOpen   bool
	lineLength int
}

func newMigrationProgress(out *os.File) *migrationProgress {
	fd := int(out.Fd())
	return &migrationProgress{
		out: out, terminal: term.IsTerminal(fd), now: time.Now,
		width: func() int {
			width, _, err := term.GetSize(fd)
			if err != nil {
				return 80
			}
			return width
		},
	}
}

func (p *migrationProgress) report(next migrate.Progress) {
	now := p.now()
	transition := !p.started || next.FromVersion != p.last.FromVersion || next.ToVersion != p.last.ToVersion
	phase := transition || next.Phase != p.last.Phase
	end := next.Total > 0 && next.Completed >= next.Total && (phase || p.last.Total <= 0 || p.last.Completed < p.last.Total)
	if !phase && !end {
		if now.Sub(p.lastDraw) < 100*time.Millisecond {
			return
		}
		// Redirected stderr gets phase boundaries and quarter milestones, not
		// a stream of animated frames on every update.
		if !p.terminal && progressQuarter(next) <= progressQuarter(p.last) {
			return
		}
	}
	if !p.started {
		fmt.Fprintln(p.out, "Upgrading history; changes are pending until all steps finish.")
		p.started = true
	}
	if transition {
		p.closeLine()
	}
	line := migrationProgressLine(next, -1)
	if p.terminal {
		width := max(0, p.width()-1) // Leave a cell free to avoid terminal autowrap.
		line = migrationProgressLine(next, width)
		padding := max(0, min(p.lineLength, width)-len(line))
		fmt.Fprintf(p.out, "\r%s%s", line, strings.Repeat(" ", padding))
		p.lineOpen = true
		p.lineLength = len(line)
	} else {
		fmt.Fprintln(p.out, line)
	}
	p.last, p.lastDraw = next, now
}

func progressQuarter(progress migrate.Progress) int {
	if progress.Total <= 0 {
		return 0
	}
	return int(4 * min(1, max(0, float64(progress.Completed)/float64(progress.Total))))
}

// migrationProgressLine uses ASCII so byte length is also terminal cell width.
// A negative width means unbounded, as used for line-oriented redirected output.
func migrationProgressLine(progress migrate.Progress, width int) string {
	phase := strings.Map(func(r rune) rune {
		if r >= ' ' && r <= '~' {
			return r
		}
		return ' '
	}, progress.Phase)
	if phase == "complete" {
		phase = "complete (pending commit)"
	}
	prefix := fmt.Sprintf("History %d->%d", progress.FromVersion, progress.ToVersion)
	status := phase
	fraction := 0.0
	if progress.Total > 0 {
		fraction = min(1, max(0, float64(progress.Completed)/float64(progress.Total)))
		percent := int(100 * fraction)
		if progress.Completed < progress.Total {
			percent = min(99, percent)
		}
		status = fmt.Sprintf("%3d%% %s", percent, phase)
	}
	line := prefix + " " + status
	if width >= 0 {
		barWidth := min(24, width-len(line)-3)
		if barWidth >= 4 && progress.Total > 0 {
			filled := int(float64(barWidth) * fraction)
			if progress.Completed < progress.Total {
				filled = min(barWidth-1, filled)
			}
			line = prefix + " [" + strings.Repeat("=", filled) + strings.Repeat("-", barWidth-filled) + "] " + status
		}
		if len(line) > width {
			line = line[:width]
		}
	}
	return strings.TrimRight(line, " ")
}

func (p *migrationProgress) closeLine() {
	if p.lineOpen {
		fmt.Fprintln(p.out)
		p.lineOpen = false
		p.lineLength = 0
	}
}

func (p *migrationProgress) finish(err error) {
	if !p.started {
		return
	}
	p.closeLine()
	if err != nil {
		fmt.Fprintln(p.out, "History migration failed.")
	} else {
		fmt.Fprintln(p.out, "History migration complete.")
	}
}
