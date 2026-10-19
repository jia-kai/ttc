package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"ttc/internal/history"
	"ttc/internal/history/migrate"

	"golang.org/x/term"
)

// startupProgress owns stderr until the frontend starts. Fractions describe
// phase-local work units, never estimated elapsed time or committed deletions.
type startupProgress struct {
	out        io.Writer
	terminal   bool
	width      func() int
	now        func() time.Time
	last       progressState
	lastDraw   time.Time
	lineLength int
}

type progressState struct {
	title, phase     string
	completed, total int64
}

func newStartupProgress(out *os.File) *startupProgress {
	fd := int(out.Fd())
	return &startupProgress{
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

func (p *startupProgress) migration(next migrate.Progress) {
	if p.last.title == "" {
		fmt.Fprintln(p.out, "Upgrading history; changes are pending until all steps finish.")
	}
	phase := next.Phase
	if phase == "complete" {
		phase = "complete (pending commit)"
	}
	p.report(progressState{fmt.Sprintf("History %d->%d", next.FromVersion, next.ToVersion), phase, next.Completed, next.Total})
}

func (p *startupProgress) maintenance(next history.MaintenanceProgress) {
	phase := map[string]string{
		"discovery":   "discovering inactive conversation families",
		"indexes":     "preparing deletion indexes",
		"candidates":  "candidates (pending commit)",
		"commit":      "committing history deletion",
		"assets":      "removing deleted conversation assets",
		"reclamation": "preparing disk reclamation",
		"checkpoint":  "checkpointing history WAL",
		"vacuum":      "compacting history database",
		"statistics":  "measuring history storage",
	}[next.Phase]
	if phase == "" {
		phase = next.Phase
	}
	p.report(progressState{"History maintenance", phase, next.Completed, next.Total})
}

func (p *startupProgress) report(next progressState) {
	now := p.now()
	transition := next.title != p.last.title
	boundary := transition || next.phase != p.last.phase ||
		(next.total > 0 && next.completed >= next.total && (p.last.total <= 0 || p.last.completed < p.last.total))
	// Redirected stderr gets phase boundaries and quarter milestones, not frames.
	if !boundary && (now.Sub(p.lastDraw) < 100*time.Millisecond ||
		(!p.terminal && progressQuarter(next) <= progressQuarter(p.last))) {
		return
	}
	if transition {
		p.closeLine()
	}
	width := -1
	if p.terminal {
		width = max(0, p.width()-1) // Leave a cell free to avoid terminal autowrap.
	}
	line := startupProgressLine(next, width)
	if p.terminal {
		padding := max(0, min(p.lineLength, width)-len(line))
		fmt.Fprintf(p.out, "\r%s%s", line, strings.Repeat(" ", padding))
		p.lineLength = len(line)
	} else {
		fmt.Fprintln(p.out, line)
	}
	p.last, p.lastDraw = next, now
}

func progressQuarter(progress progressState) int {
	if progress.total <= 0 {
		return 0
	}
	return int(4 * min(1, max(0, float64(progress.completed)/float64(progress.total))))
}

// startupProgressLine uses ASCII so byte length equals terminal cell width.
// Negative width is unbounded, for line-oriented redirected stderr.
func startupProgressLine(progress progressState, width int) string {
	clean := func(text string) string {
		return strings.Map(func(r rune) rune {
			if r >= ' ' && r <= '~' {
				return r
			}
			return ' '
		}, text)
	}
	status := progress.phase + " ..."
	fraction := 0.0
	if progress.total > 0 {
		fraction = min(1, max(0, float64(progress.completed)/float64(progress.total)))
		percent := int(100 * fraction)
		if progress.completed < progress.total {
			percent = min(99, percent)
		}
		status = fmt.Sprintf("%3d%% %s (%d/%d)", percent, progress.phase, max(0, progress.completed), progress.total)
	}
	prefix, status := clean(progress.title), clean(status)
	bar := ""
	barWidth := min(24, width-len(prefix)-len(status)-4)
	if progress.total > 0 && barWidth >= 4 {
		filled := int(float64(barWidth) * fraction)
		if progress.completed < progress.total {
			filled = min(barWidth-1, filled)
		}
		bar = "[" + strings.Repeat("=", filled) + strings.Repeat("-", barWidth-filled) + "] "
	}
	line := prefix + " " + bar + status
	if width >= 0 && len(line) > width {
		line = line[:width]
	}
	return strings.TrimRight(line, " ")
}

func (p *startupProgress) closeLine() {
	if p.terminal && p.last.title != "" {
		fmt.Fprintln(p.out)
		p.lineLength = 0
	}
}

func (p *startupProgress) finish(title string, err error) {
	if p.last.title == "" {
		return
	}
	p.closeLine()
	status := "complete"
	if err != nil {
		status = "failed"
	}
	fmt.Fprintf(p.out, "%s %s.\n", title, status)
	p.last = progressState{}
}
