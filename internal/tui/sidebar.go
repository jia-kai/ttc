package tui

import (
	"fmt"
	"strings"
	"time"
	"ttc/internal/workspace"

	"ttc/internal/jobs"
	"ttc/internal/render"
	"ttc/internal/session"
	"ttc/internal/version"

	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
)

type sidebarSection struct {
	title       string
	collapsed   bool
	scroll      int
	rows        []string // Context rows only; jobs and timers retain typed metadata.
	top, height int
}
type sidebar struct {
	sections            [3]sidebarSection
	overlay             bool
	left, width, height int
	focus               int
	workspace           workspace.GitInfo
	sessionName         string
	workspaceHeight     int
	texts               map[string]*rollingText // Visible identities only; reused across metadata refreshes.
	jobs                []jobs.Snapshot         // Copied metadata in the immutable launch order supplied by Jobs.Live.
	timers              []session.TimerView
}
type sidebarAction struct {
	jobID     string
	timerID   string
	workspace bool
}

func newSidebar() *sidebar {
	return &sidebar{sections: [3]sidebarSection{{title: "Context usage"}, {title: "Running jobs"}, {title: "Timers"}}}
}
func (b *sidebar) update(u session.ContextUsage, jobs []jobs.Snapshot, timers []session.TimerView) {
	c := &b.sections[0]
	c.rows = append(c.rows[:0], "No request yet")
	if u.Limit > 0 {
		c.rows = append(c.rows[:0], u.Model)
		if v := u.Reported; v != nil {
			c.rows = append(c.rows, "Last input · reported")
			if v.Model != u.Model {
				c.rows = append(c.rows, v.Model)
			}
			c.rows = append(c.rows, fmt.Sprintf("Input %d · cached %s", v.Tokens.InputTokens, optionalTokens(v.Tokens.CachedInputTokens)))
			if cached := v.Tokens.CachedInputTokens; cached != nil && *cached <= v.Tokens.InputTokens {
				c.rows = append(c.rows, fmt.Sprintf("Uncached input %d", v.Tokens.InputTokens-*cached))
			}
		} else {
			c.rows = append(c.rows, "Reported usage unavailable")
		}
	}
	if totals := u.Totals; totals.Requests > 0 {
		c.rows = append(c.rows, "Run output · all agents", fmt.Sprintf("Reported %d/%d requests", totals.ReportedRequests, totals.Requests), fmt.Sprintf("Output %d · reasoning %s", totals.Tokens.OutputTokens, optionalTokens(totals.Tokens.ReasoningOutputTokens)))
	}
	if u.Limit > 0 {
		input, qualifier := u.Input, "estimated"
		if u.RequestID > 0 && u.Reported != nil && u.Reported.RequestID == u.RequestID {
			input, qualifier = u.Reported.Tokens.InputTokens, "reported input"
		}
		used := min(16, (input+u.Reserved)*16/max(1, u.Limit))
		c.rows = append(c.rows, fmt.Sprintf("Context %.1f%%", float64(input+u.Reserved)*100/float64(u.Limit)), fmt.Sprintf("%s%s", strings.Repeat("━", used), strings.Repeat("─", 16-used)), fmt.Sprintf("%d / %d tokens", input+u.Reserved, u.Limit), qualifier+" + reserve", fmt.Sprintf("Input %d · reserve %s", input, shortTokens(u.Reserved)), "Breakdown · estimated")
		for _, p := range u.Parts {
			c.rows = append(c.rows, fmt.Sprintf("%-19s %7s", p.Name, shortTokens(p.Tokens)))
		}
	}
	clear(c.rows[len(c.rows):cap(c.rows)])
	if len(jobs) < len(b.jobs) {
		clear(b.jobs[len(jobs):])
	}
	b.jobs = append(b.jobs[:0], jobs...)
	if len(timers) < len(b.timers) {
		clear(b.timers[len(timers):])
	}
	b.timers = append(b.timers[:0], timers...)
}

func (b *sidebar) sectionCount(index int) int {
	switch index {
	case 1:
		return len(b.jobs)
	case 2:
		return len(b.timers)
	default:
		return len(b.sections[index].rows)
	}
}

func sidebarJobPrefix(v jobs.Snapshot, now time.Time) string {
	elapsed := ""
	if at, err := time.Parse(time.RFC3339Nano, v.StartedAt); err == nil {
		elapsed = " · " + shortDuration(now.Sub(at))
	}
	kind := v.Kind
	if kind == "subagent" {
		kind = "agent"
	}
	return "● " + kind + elapsed + " · "
}

func sidebarTimerSuffix(v session.TimerView, now time.Time) string {
	when := v.NextAt
	if at, err := time.Parse(time.RFC3339Nano, v.NextAt); err == nil {
		when = "in " + shortDuration(at.Sub(now))
	}
	return " · " + when
}
func (b *sidebar) bounds(w, h int, fullscreen bool) int {
	b.height = h
	b.width = 0
	if fullscreen && !b.overlay {
		return w
	}
	if w >= 100 || b.overlay {
		b.width = min(34, max(1, w-2))
		b.left = w - b.width
		if b.overlay {
			return w
		}
		return b.left
	}
	return w
}
func (b *sidebar) draw(s tcell.Screen) {
	b.drawAt(s, time.Now())
}

func (b *sidebar) drawAt(s tcell.Screen, now time.Time) {
	if b.width == 0 {
		b.texts = nil
		return
	}
	visible := make(map[string]*rollingText)
	drawText := func(key string, x, y, width int, source string, style tcell.Style) {
		if width <= 0 || y < 0 || y >= b.height {
			return
		}
		text := b.texts[key]
		if text == nil || text.source != source {
			text = newRollingText(source)
		}
		visible[key] = text
		text.draw(s, x, y, width, now, style)
	}
	defer func() { b.texts = visible }()
	style := tcell.StyleDefault.Background(tcell.GetColor(render.SurfaceColor)).Foreground(tcell.GetColor(render.TextColor))
	for y := range b.height {
		put(s, b.left, y, b.width, strings.Repeat(" ", b.width), style)
	}
	expanded := 0
	for _, v := range b.sections {
		if !v.collapsed {
			expanded++
		}
	}
	top := 0
	if b.sessionName != "" {
		drawText("session-header", b.left+1, top, b.width-2, "TTC "+version.Version, style.Foreground(tcell.GetColor(render.CyanColor)).Bold(true))
		top++
		drawText("session-name", b.left+1, top, b.width-2, b.sessionName, style.Foreground(tcell.GetColor(render.BlueColor)))
		top++
	}
	if b.workspace.Cwd != "" {
		drawText("workspace-header", b.left+1, top, b.width-2, "WORKSPACE", style.Foreground(tcell.GetColor(render.CyanColor)).Bold(true))
		top++
		for _, item := range [][2]string{{"cwd", b.workspace.Cwd}, {"git", b.workspace.Repo}, {"⎇", b.workspace.Branch}} {
			if item[1] == "" {
				continue
			}
			put(s, b.left+1, top, 4, item[0], style.Foreground(tcell.GetColor(render.MutedColor)))
			valueStyle := style
			if item[0] == "⎇" {
				valueStyle = style.Foreground(tcell.GetColor(render.BlueColor))
			}
			drawText("workspace:"+item[0], b.left+5, top, b.width-6, item[1], valueStyle)
			top++
		}
		put(s, b.left+1, top, max(0, b.width-2), strings.Repeat("─", max(0, b.width-2)), style.Foreground(tcell.GetColor(render.BorderColor)))
		top++
	}
	b.workspaceHeight = top
	available := max(0, b.height-3-top)
	remaining := expanded
	for i := range b.sections {
		v := &b.sections[i]
		v.top = top
		v.height = 0
		arrow := "▾"
		if v.collapsed {
			arrow = "▸"
		}
		count := b.sectionCount(i)
		title := fmt.Sprintf("%s %s", arrow, v.title)
		if i != 0 {
			title += fmt.Sprintf(" · %d", count)
		}
		drawText("header:"+v.title, b.left+1, top, b.width-2, title, style.Foreground(tcell.GetColor(render.CyanColor)).Bold(true).Reverse(b.overlay && b.focus == i))
		top++
		if v.collapsed {
			continue
		}
		quota := 0
		if remaining > 0 {
			quota = available / remaining
		}
		v.height = min(max(1, count), quota)
		available -= v.height
		remaining--
		v.scroll = min(max(0, v.scroll), max(0, count-v.height))
		if count == 0 && v.height > 0 {
			put(s, b.left+2, top, b.width-3, "—", style.Foreground(tcell.GetColor(render.MutedColor)))
		}
		for n := range v.height {
			if b.width <= 2 || top+n < 0 || top+n >= b.height {
				continue
			}
			if index := v.scroll + n; index < count {
				if i == 1 {
					job := b.jobs[index]
					prefix := sidebarJobPrefix(job, now)
					put(s, b.left+1, top+n, b.width-2, prefix, style)
					prefixCells := runewidth.StringWidth(prefix)
					drawText("job:"+job.ID, b.left+1+prefixCells, top+n, b.width-2-prefixCells, job.Label, style)
				} else if i == 2 {
					timer := b.timers[index]
					suffix := sidebarTimerSuffix(timer, now)
					suffixCells := min(max(0, b.width-2), runewidth.StringWidth(suffix))
					labelCells := b.width - 2 - suffixCells
					drawText("timer:"+timer.ID, b.left+1, top+n, labelCells, timer.Name, style)
					put(s, b.left+1+max(0, labelCells), top+n, suffixCells, suffix, style)
				} else {
					drawText(fmt.Sprintf("context:%d", index), b.left+1, top+n, b.width-2, v.rows[index], style)
				}
			}
		}
		drawScrollBar(s, b.left+b.width-1, top, v.height, count, v.scroll, style.Foreground(tcell.GetColor(render.MutedColor)))
		top += v.height
	}
}

func (b *sidebar) detail() string {
	if b.sessionName == "" {
		return b.workspace.Detail()
	}
	return "Session:\n" + b.sessionName + "\n\n" + b.workspace.Detail()
}

// mouse consumes sidebar clicks/wheels and returns a live job or timer to inspect.
func (b *sidebar) mouse(ev *tcell.EventMouse) (bool, sidebarAction) {
	x, y := ev.Position()
	if b.width == 0 || x < b.left || x >= b.left+b.width || y < 0 || y >= b.height {
		return false, sidebarAction{}
	}
	if y < b.workspaceHeight {
		return true, sidebarAction{workspace: ev.Buttons()&tcell.Button1 != 0}
	}
	for i := range b.sections {
		v := &b.sections[i]
		if y == v.top {
			b.focus = i
			if ev.Buttons()&tcell.Button1 != 0 {
				v.collapsed = !v.collapsed
			}
			return true, sidebarAction{}
		}
		if !v.collapsed && y > v.top && y <= v.top+v.height {
			b.focus = i
			switch {
			case ev.Buttons()&tcell.WheelUp != 0:
				v.scroll = max(0, v.scroll-3)
			case ev.Buttons()&tcell.WheelDown != 0:
				v.scroll = min(max(0, b.sectionCount(i)-v.height), v.scroll+3)
			case ev.Buttons()&tcell.Button1 != 0 && (i == 1 || i == 2):
				index := v.scroll + y - v.top - 1
				if i == 1 && index < len(b.jobs) {
					return true, sidebarAction{jobID: b.jobs[index].ID}
				}
				if i == 2 && index < len(b.timers) {
					return true, sidebarAction{timerID: b.timers[index].ID}
				}
			}
			return true, sidebarAction{}
		}
	}
	return true, sidebarAction{}
}

func optionalTokens(n *int) string {
	if n == nil {
		return "—"
	}
	return fmt.Sprint(*n)
}

func shortTokens(n int) string {
	if n >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(n)/1000000)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}
func shortDuration(d time.Duration) string {
	seconds := max(0, int(d.Seconds()))
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	if seconds < 3600 {
		return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%dh%02dm", seconds/3600, seconds/60%60)
}

// key provides sidebar navigation in the narrow-pane overlay. Tab changes list;
// arrows scroll or collapse/expand that list. Composer recall resumes on close.
func (b *sidebar) key(ev *tcell.EventKey) bool {
	v := &b.sections[b.focus]
	switch ev.Key() {
	case tcell.KeyTab:
		b.focus = (b.focus + 1) % len(b.sections)
	case tcell.KeyUp:
		v.scroll = max(0, v.scroll-1)
	case tcell.KeyDown:
		v.scroll = min(max(0, b.sectionCount(b.focus)-v.height), v.scroll+1)
	case tcell.KeyPgUp:
		v.scroll = max(0, v.scroll-v.height)
	case tcell.KeyPgDn:
		v.scroll = min(max(0, b.sectionCount(b.focus)-v.height), v.scroll+v.height)
	case tcell.KeyLeft:
		v.collapsed = true
	case tcell.KeyRight:
		v.collapsed = false
	case tcell.KeyEnter:
		v.collapsed = !v.collapsed
	default:
		return false
	}
	return true
}
