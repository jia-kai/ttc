package tui

import (
	"fmt"
	"scicode/internal/workspace"
	"strings"
	"time"

	"scicode/internal/jobs"
	"scicode/internal/render"
	"scicode/internal/session"

	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
)

type sidebarSection struct {
	title       string
	collapsed   bool
	scroll      int
	rows        []string
	ids         []string
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
}
type sidebarAction struct {
	jobID     string
	workspace bool
}

func newSidebar() *sidebar {
	return &sidebar{sections: [3]sidebarSection{{title: "Context usage"}, {title: "Running jobs"}, {title: "Timers"}}}
}
func (b *sidebar) update(u session.ContextUsage, jobs []jobs.Snapshot, timers []session.TimerView) {
	c := &b.sections[0]
	c.rows = []string{"No request yet"}
	if u.Limit > 0 {
		input, qualifier := u.Input, "estimated"
		if u.RequestID > 0 && u.Reported != nil && u.Reported.RequestID == u.RequestID {
			input, qualifier = u.Reported.Tokens.InputTokens, "reported input"
		}
		used := min(16, (input+u.Reserved)*16/max(1, u.Limit))
		c.rows = []string{render.Clean(u.Model)}
		if v := u.Reported; v != nil {
			c.rows = append(c.rows, "Last response · reported")
			if v.Model != u.Model {
				c.rows = append(c.rows, render.Clean(v.Model))
			}
			c.rows = append(c.rows, fmt.Sprintf("Input %d · cached %s", v.Tokens.InputTokens, optionalTokens(v.Tokens.CachedInputTokens)), fmt.Sprintf("Output %d · reasoning %s", v.Tokens.OutputTokens, optionalTokens(v.Tokens.ReasoningOutputTokens)))
			if cached := v.Tokens.CachedInputTokens; cached != nil && *cached <= v.Tokens.InputTokens {
				c.rows = append(c.rows, fmt.Sprintf("Uncached input %d", v.Tokens.InputTokens-*cached))
			}
		} else {
			c.rows = append(c.rows, "Reported usage unavailable")
		}
		c.rows = append(c.rows, fmt.Sprintf("Context %.1f%%", float64(input+u.Reserved)*100/float64(u.Limit)), fmt.Sprintf("%s%s", strings.Repeat("━", used), strings.Repeat("─", 16-used)), fmt.Sprintf("%d / %d tokens", input+u.Reserved, u.Limit), qualifier+" + reserve", fmt.Sprintf("Input %d · reserve %s", input, shortTokens(u.Reserved)), "Breakdown · estimated")
		for _, p := range u.Parts {
			c.rows = append(c.rows, fmt.Sprintf("%-19s %7s", p.Name, shortTokens(p.Tokens)))
		}
	}
	if totals := u.Totals; totals.Requests > 0 {
		c.rows = append(c.rows, "Run totals · all agents", fmt.Sprintf("Reported %d/%d requests", totals.ReportedRequests, totals.Requests), fmt.Sprintf("Input %d · cached %s", totals.Tokens.InputTokens, optionalTokens(totals.Tokens.CachedInputTokens)), fmt.Sprintf("Output %d · reasoning %s", totals.Tokens.OutputTokens, optionalTokens(totals.Tokens.ReasoningOutputTokens)))
		if cached := totals.Tokens.CachedInputTokens; cached != nil && *cached <= totals.Tokens.InputTokens {
			c.rows = append(c.rows, fmt.Sprintf("Uncached input %d", totals.Tokens.InputTokens-*cached))
		}
		if cached, written := totals.Tokens.CachedInputTokens, totals.Tokens.CacheWriteTokens; cached != nil && written != nil && *cached+*written <= totals.Tokens.InputTokens {
			c.rows = append(c.rows, fmt.Sprintf("Ordinary input %d", totals.Tokens.InputTokens-*cached-*written))
		}
	}
	j := &b.sections[1]
	j.rows = nil
	j.ids = nil
	for _, v := range jobs {
		elapsed := ""
		if at, err := time.Parse(time.RFC3339, v.StartedAt); err == nil {
			elapsed = " · " + shortDuration(time.Since(at))
		}
		kind := v.Kind
		if kind == "subagent" {
			kind = "agent"
		}
		j.rows = append(j.rows, "● "+kind+elapsed+" · "+strings.Join(strings.Fields(render.Clean(v.Label)), " "))
		j.ids = append(j.ids, v.ID)
	}
	q := &b.sections[2]
	q.rows = nil
	q.ids = nil
	for _, v := range timers {
		when := v.NextAt
		if at, err := time.Parse(time.RFC3339Nano, v.NextAt); err == nil {
			when = "in " + shortDuration(max(time.Duration(0), time.Until(at)))
		}
		q.rows = append(q.rows, render.Clean(v.Name)+" · "+when)
		q.ids = append(q.ids, v.ID)
	}
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
	if b.width == 0 {
		return
	}
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
		put(s, b.left+1, top, b.width-2, "SESSION", style.Foreground(tcell.GetColor(render.CyanColor)).Bold(true))
		top++
		name := strings.Join(strings.Fields(render.Clean(b.sessionName)), " ")
		put(s, b.left+1, top, b.width-2, runewidth.Truncate(name, max(0, b.width-2), "…"), style.Foreground(tcell.GetColor(render.BlueColor)))
		top++
	}
	if b.workspace.Cwd != "" {
		put(s, b.left+1, top, b.width-2, "WORKSPACE", style.Foreground(tcell.GetColor(render.CyanColor)).Bold(true))
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
			put(s, b.left+5, top, b.width-6, leftClip(render.Clean(item[1]), b.width-6), valueStyle)
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
		count := len(v.rows)
		title := fmt.Sprintf("%s %s", arrow, v.title)
		if i != 0 {
			title += fmt.Sprintf(" · %d", count)
		}
		put(s, b.left+1, top, b.width-2, title, style.Foreground(tcell.GetColor(render.CyanColor)).Bold(true).Reverse(b.overlay && b.focus == i))
		top++
		if v.collapsed {
			continue
		}
		quota := 0
		if remaining > 0 {
			quota = available / remaining
		}
		v.height = min(max(1, len(v.rows)), quota)
		available -= v.height
		remaining--
		v.scroll = min(max(0, v.scroll), max(0, len(v.rows)-v.height))
		if len(v.rows) == 0 && v.height > 0 {
			put(s, b.left+2, top, b.width-3, "—", style.Foreground(tcell.GetColor(render.MutedColor)))
		}
		for n := range v.height {
			if index := v.scroll + n; index < len(v.rows) {
				put(s, b.left+1, top+n, b.width-2, v.rows[index], style)
			}
		}
		drawScrollBar(s, b.left+b.width-1, top, v.height, len(v.rows), v.scroll, style.Foreground(tcell.GetColor(render.MutedColor)))
		top += v.height
	}
}

func (b *sidebar) detail() string {
	if b.sessionName == "" {
		return b.workspace.Detail()
	}
	return "Session:\n" + b.sessionName + "\n\n" + b.workspace.Detail()
}

// mouse consumes sidebar clicks/wheels and returns a live job to inspect.
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
				v.scroll = min(max(0, len(v.rows)-v.height), v.scroll+3)
			case ev.Buttons()&tcell.Button1 != 0 && i == 1:
				index := v.scroll + y - v.top - 1
				if index < len(v.ids) {
					return true, sidebarAction{jobID: v.ids[index]}
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
func leftClip(text string, cells int) string {
	if cells <= 0 {
		return ""
	}
	if runewidth.StringWidth(text) <= cells {
		return text
	}
	return runewidth.TruncateLeft(text, runewidth.StringWidth(text)-cells+1, "…")
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
		v.scroll = min(max(0, len(v.rows)-v.height), v.scroll+1)
	case tcell.KeyPgUp:
		v.scroll = max(0, v.scroll-v.height)
	case tcell.KeyPgDn:
		v.scroll = min(max(0, len(v.rows)-v.height), v.scroll+v.height)
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
