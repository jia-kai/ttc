package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"ttc/internal/workspace"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/jobs"
	"ttc/internal/provider"
	"ttc/internal/session"
	"ttc/internal/version"
)

func TestSidebarVersionHeader(t *testing.T) {
	for _, width := range []int{120, 80} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			s := tcell.NewSimulationScreen("UTF-8")
			if err := s.Init(); err != nil {
				t.Fatal(err)
			}
			defer s.Fini()
			s.SetSize(width, 24)
			b := newSidebar()
			b.sessionName = "Research session"
			b.overlay = width < 100
			b.bounds(width, 24, false)
			b.draw(s)
			if got := strings.TrimSpace(sidebarScreenText(s, b.left+1, 0, b.width-2)); got != "TTC "+version.Version {
				t.Fatalf("sidebar header = %q, want TTC %s", got, version.Version)
			}
			if got := strings.TrimSpace(sidebarScreenText(s, b.left+1, 1, b.width-2)); got != b.sessionName {
				t.Fatalf("session name = %q, want %q", got, b.sessionName)
			}
			if _, action := b.mouse(tcell.NewEventMouse(b.left+1, 0, tcell.Button1, 0)); !action.workspace {
				t.Fatal("version header no longer opens session and workspace details")
			}
		})
	}
}

func TestUncachedCounterDoesNotRequireCacheWriteCounter(t *testing.T) {
	cached := 800
	u := session.ContextUsage{Limit: 10000, Input: 1000, Reported: &session.ReportedUsage{Tokens: provider.Usage{InputTokens: 1000, CachedInputTokens: &cached}}, Totals: session.UsageTotals{Requests: 1, ReportedRequests: 1, Tokens: provider.Usage{InputTokens: 1000, CachedInputTokens: &cached}}}
	b := newSidebar()
	b.update(u, nil, nil)
	text := strings.Join(b.sections[0].rows, "\n")
	if strings.Count(text, "Uncached input 200") != 1 || !strings.Contains(text, "Input 1000") {
		t.Fatal("cached/full input remain indistinguishable", text)
	}
	if strings.Contains(text, "Cache writes") {
		t.Fatal("removed sidebar counter still visible", text)
	}
}

func sidebarScreenText(s tcell.Screen, x, y, width int) string {
	var text strings.Builder
	for col := 0; col < width; {
		r, combining, _, cells := s.GetContent(x+col, y)
		text.WriteRune(r)
		for _, mark := range combining {
			text.WriteRune(mark)
		}
		col += max(1, cells)
	}
	return text.String()
}

func TestSidebarJobMarqueeRefreshPrefixAndClickIdentity(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(120, 24)
	b := newSidebar()
	js := []jobs.Snapshot{
		{ID: "shell-id", Kind: "shell", Label: "abcdefghijklmnopqrstuvwxyz-tail"},
		{ID: "agent-id", Kind: "subagent", Label: "ABCDEFGHIJKLMNOPQRSTUVWXYZ-tail"},
		{ID: "short-id", Kind: "shell", Label: "short"},
	}
	b.update(session.ContextUsage{}, js, nil)
	b.bounds(120, 24, false)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.drawAt(s, at)
	section := &b.sections[1]
	for i, prefix := range []string{"● shell · ", "● agent · "} {
		got := sidebarScreenText(s, b.left+1, section.top+1+i, b.width-2)
		want := prefix + js[i].Label[:b.width-2-len([]rune(prefix))]
		if got != want {
			t.Fatalf("initial row %d: %q, want %q", i, got, want)
		}
	}
	cached := b.texts["job:shell-id"]
	js[0].Stdout = "new output"
	b.update(session.ContextUsage{}, js, nil)
	b.drawAt(s, at.Add(1250*time.Millisecond))
	if b.texts["job:shell-id"] != cached {
		t.Fatal("metadata refresh reset animation")
	}
	for i, prefix := range []string{"● shell · ", "● agent · "} {
		got := sidebarScreenText(s, b.left+1, section.top+1+i, b.width-2)
		want := prefix + js[i].Label[1:1+b.width-2-len([]rune(prefix))]
		if got != want {
			t.Fatalf("moving row %d: %q, want %q", i, got, want)
		}
		if _, action := b.mouse(tcell.NewEventMouse(b.left+20, section.top+1+i, tcell.Button1, 0)); action.jobID != js[i].ID {
			t.Fatal("animation changed click target", action)
		}
	}
	if got := sidebarScreenText(s, b.left+1, section.top+3, b.width-2); !strings.HasPrefix(got, "● shell · short ") {
		t.Fatal("fitting label moved", got)
	}
	width := b.width - 2 - len([]rune(sidebarJobPrefix(b.jobs[0], at)))
	end := at.Add(time.Duration(4+cached.cells-width) * 250 * time.Millisecond)
	b.drawAt(s, end)
	if got := sidebarScreenText(s, b.left+1, section.top+1, b.width-2); !strings.HasSuffix(got, "-tail") {
		t.Fatal("end of label inaccessible", got)
	}
	if rollingTextOffset(end.Sub(at)+500*time.Millisecond, cached.cells-width) != cached.cells-width {
		t.Fatal("label end does not pause")
	}
}

func TestRollingTextUnicodeCells(t *testing.T) {
	if text := newRollingText("path with  spaces\nnext\tvalue\x00").text; text != "path with  spaces next    value" {
		t.Fatal("single-line sanitization changed readable spacing or retained controls", text)
	}
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(10, 1)
	l := &rollingText{text: "界e\u0301界末"}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.draw(s, 0, 0, 4, at, tcell.StyleDefault)
	if r, _, _, width := s.GetContent(0, 0); r != '界' || width != 2 {
		t.Fatal("CJK glyph split", r, width)
	}
	if r, combining, _, _ := s.GetContent(2, 0); r != 'e' || len(combining) != 1 || combining[0] != '\u0301' {
		t.Fatal("combining cluster split", r, combining)
	}
	s.Clear()
	l.draw(s, 0, 0, 4, at.Add(1250*time.Millisecond), tcell.StyleDefault)
	if got := sidebarScreenText(s, 0, 0, 4); got != " e\u0301界" {
		t.Fatalf("partial wide glyph not clipped safely: %q", got)
	}
	s.Clear()
	l.draw(s, 0, 0, 1, at, tcell.StyleDefault)
	if r, _, _, _ := s.GetContent(0, 0); r != ' ' {
		t.Fatal("wide glyph drawn in one cell", r)
	}
	s.Clear()
	emoji := &rollingText{text: "👩‍💻abc"}
	emoji.draw(s, 0, 0, 3, at, tcell.StyleDefault)
	if r, combining, _, width := s.GetContent(0, 0); r != '👩' || string(combining) != "‍💻" || width != 2 {
		t.Fatal("emoji cluster split", r, combining, width)
	}
}

func TestSidebarSharedRollingTextShowsAllOverflowingFields(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(120, 60)
	b := newSidebar()
	b.sessionName = strings.Repeat("Session ", 6) + "session-tail"
	b.workspace = workspace.GitInfo{Cwd: "/" + strings.Repeat("project/", 6) + "cwd-tail", Repo: "/" + strings.Repeat("repository/", 6) + "repo-tail", Branch: strings.Repeat("research-", 6) + "branch-tail"}
	usage := session.ContextUsage{Model: strings.Repeat("Model ", 6) + "model-tail", Limit: 10000}
	timers := []session.TimerView{{ID: "timer-id", Name: strings.Repeat("Timer ", 6) + "timer-tail", NextAt: "soon"}}
	b.update(usage, nil, timers)
	b.bounds(120, 60, false)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.drawAt(s, at)
	for _, item := range []struct {
		key, tail string
		x, y, w   int
	}{
		{"session-name", "session-tail", b.left + 1, 1, b.width - 2},
		{"workspace:cwd", "cwd-tail", b.left + 5, 3, b.width - 6},
		{"workspace:git", "repo-tail", b.left + 5, 4, b.width - 6},
		{"workspace:⎇", "branch-tail", b.left + 5, 5, b.width - 6},
		{"context:0", "model-tail", b.left + 1, b.sections[0].top + 1, b.width - 2},
		{"timer:timer-id", "timer-tail", b.left + 1, b.sections[2].top + 1, b.width - 2 - len([]rune(" · soon"))},
	} {
		text := b.texts[item.key]
		if text == nil || text.cells <= item.w {
			t.Fatal("overflow field did not use the shared widget", item.key)
		}
		b.drawAt(s, at.Add(time.Duration(4+text.cells-item.w)*250*time.Millisecond))
		if got := sidebarScreenText(s, item.x, item.y, item.w); !strings.HasSuffix(got, item.tail) {
			t.Fatal("text tail not visible", item.key, got)
		}
	}
	// A changing timer countdown is separate from its title's animation epoch.
	cached := b.texts["timer:timer-id"]
	timers[0].NextAt = "later"
	b.update(usage, nil, timers)
	b.drawAt(s, at.Add(2*time.Second))
	if b.texts["timer:timer-id"] != cached {
		t.Fatal("timer metadata refresh reset its title animation")
	}
	b.sessionName = "Renamed"
	b.drawAt(s, at.Add(3*time.Second))
	if got := sidebarScreenText(s, b.left+1, 1, b.width-2); !strings.HasPrefix(got, "Renamed ") {
		t.Fatal("replacement text retained old offset", got)
	}
}

func TestSidebarMarqueeKeepsElapsedPrefixAndNarrowClickTarget(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(120, 15)
	b := newSidebar()
	b.update(session.ContextUsage{}, []jobs.Snapshot{{ID: "job", Kind: "shell", StartedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), Label: strings.Repeat("abc", 20)}}, nil)
	b.bounds(120, 15, false)
	at := time.Now()
	b.drawAt(s, at)
	prefix := sidebarJobPrefix(b.jobs[0], at)
	if !strings.Contains(prefix, " · 1m") {
		t.Fatal("elapsed time missing from fixed prefix", prefix)
	}
	b.drawAt(s, at.Add(2*time.Second))
	if got := sidebarScreenText(s, b.left+1, b.sections[1].top+1, b.width-2); !strings.HasPrefix(got, sidebarJobPrefix(b.jobs[0], at.Add(2*time.Second))) {
		t.Fatal("elapsed prefix scrolled", got)
	}
	b.overlay = true
	s.SetSize(12, 15)
	b.bounds(12, 15, false)
	b.drawAt(s, at.Add(3*time.Second))
	if _, action := b.mouse(tcell.NewEventMouse(b.left+1, b.sections[1].top+1, tcell.Button1, 0)); action.jobID != "job" {
		t.Fatal("prefix-only narrow row lost click identity", action)
	}
}

func TestSidebarMarqueeOnlyIndexesVisibleLabels(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(120, 6)
	b := newSidebar()
	js := []jobs.Snapshot{{ID: "one", Kind: "shell", Label: strings.Repeat("x", 1000)}, {ID: "two", Kind: "shell", Label: strings.Repeat("y", 1000)}}
	b.update(session.ContextUsage{}, js, nil)
	at := time.Now()
	b.bounds(120, 6, true)
	b.drawAt(s, at)
	if len(b.texts) != 0 {
		t.Fatal("fullscreen hidden labels indexed")
	}
	b.bounds(120, 6, false)
	b.drawAt(s, at)
	if b.texts["job:one"] == nil || b.texts["job:one"].glyphs == nil || b.texts["job:two"] != nil {
		t.Fatal("indexing was not limited to visible rows")
	}
	b.overlay = true
	s.SetSize(10, 6)
	b.bounds(10, 6, false)
	b.drawAt(s, at.Add(time.Second))
	if b.texts["job:two"] != nil {
		t.Fatal("narrow pane indexed invisible label")
	}
	b.update(session.ContextUsage{}, js[:1], nil)
	b.drawAt(s, at.Add(2*time.Second))
	if b.texts["job:two"] != nil {
		t.Fatal("completed job cache retained")
	}
}

func TestSidebarVisibleCachesReleasedWhenHiddenOrCompleted(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(120, 24)
	for _, mode := range []string{"narrow", "fullscreen", "completed"} {
		t.Run(mode, func(t *testing.T) {
			b := newSidebar()
			b.update(session.ContextUsage{}, []jobs.Snapshot{{ID: "visible", Kind: "shell", Label: strings.Repeat("界", 1000)}}, nil)
			b.bounds(120, 24, false)
			b.draw(s)
			if text := b.texts["job:visible"]; text == nil || len(text.glyphs) == 0 {
				t.Fatal("visible job was not indexed")
			}
			switch mode {
			case "narrow":
				b.bounds(80, 24, false)
			case "fullscreen":
				b.bounds(120, 24, true)
			case "completed":
				b.update(session.ContextUsage{}, nil, nil)
			}
			b.draw(s)
			if b.texts["job:visible"] != nil {
				t.Fatal("invisible job retained its grapheme cache")
			}
			if mode != "completed" && len(b.texts) != 0 {
				t.Fatal("hidden sidebar retained text caches")
			}
		})
	}
}

func TestSidebarMetadataCopyIsLazyAndReusesStorage(t *testing.T) {
	b := newSidebar()
	malformed := strings.Repeat("not-a-timestamp\x00", 100)
	js := []jobs.Snapshot{{ID: "oldest", Kind: "shell", StartedAt: malformed, Label: "raw\x00\nlabel"}, {ID: "newest", Kind: "subagent", StartedAt: malformed, Label: "other"}}
	timers := []session.TimerView{{ID: "timer", Name: "raw\x00\ntimer", NextAt: malformed}}
	b.update(session.ContextUsage{}, js, timers)
	jobStorage, timerStorage := &b.jobs[0], &b.timers[0]
	if allocations := testing.AllocsPerRun(100, func() { b.update(session.ContextUsage{}, js, timers) }); allocations != 0 {
		t.Fatalf("metadata refresh derived labels or allocated replacement slices: %g allocations", allocations)
	}
	if &b.jobs[0] != jobStorage || &b.timers[0] != timerStorage {
		t.Fatal("metadata storage was not reused")
	}
	js[0].Label = "mutated"
	timers[0].Name = "mutated"
	if b.jobs[0].Label != "raw\x00\nlabel" || b.jobs[1].ID != "newest" || b.timers[0].Name != "raw\x00\ntimer" {
		t.Fatal("metadata was cleaned, reordered, or retained caller slice storage")
	}
	if len(b.sections[1].rows) != 0 || len(b.sections[2].rows) != 0 || len(b.texts) != 0 {
		t.Fatal("metadata refresh eagerly derived rows or text widgets")
	}
	b.update(session.ContextUsage{}, js[:1], nil)
	if jobStorage.ID != "oldest" || b.jobs[:cap(b.jobs)][1].ID != "" || timerStorage.ID != "" {
		t.Fatal("removed metadata retained in reused slice storage")
	}
}

func TestSidebarDerivedWorkBoundedByVisibleRows(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(120, 6)
	b := newSidebar()
	js := make([]jobs.Snapshot, 2000)
	timers := make([]session.TimerView, len(js))
	for i := range js {
		js[i] = jobs.Snapshot{ID: fmt.Sprint(i), Kind: "shell", Label: "raw\x00\nlabel", StartedAt: "invalid timestamp"}
		timers[i] = session.TimerView{ID: fmt.Sprint(i), Name: "raw\x00\ntimer", NextAt: "invalid timestamp"}
	}
	b.bounds(120, 6, false)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	allocations := func(count int) float64 {
		b.update(session.ContextUsage{}, js[:count], timers[:count])
		b.drawAt(s, at)
		return testing.AllocsPerRun(20, func() { b.drawAt(s, at) })
	}
	one, many := allocations(1), allocations(len(js))
	if many > one+10 {
		t.Fatalf("offscreen metadata increased derived work: one=%g, many=%g allocations", one, many)
	}
	if b.texts["job:0"] == nil || b.texts["job:1"] != nil || b.texts["timer:1"] != nil {
		t.Fatal("offscreen malformed metadata created widgets")
	}
	b.sections[1].collapsed = true
	b.sections[2].collapsed = true
	b.drawAt(s, at)
	for key := range b.texts {
		if strings.HasPrefix(key, "job:") || strings.HasPrefix(key, "timer:") {
			t.Fatal("collapsed metadata created widgets", key)
		}
	}
}

func TestSidebarCountdownAndElapsedUseDrawTime(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	job := jobs.Snapshot{Kind: "subagent", StartedAt: at.Add(-time.Minute).Format(time.RFC3339Nano)}
	timer := session.TimerView{NextAt: at.Add(30 * time.Second).Format(time.RFC3339Nano)}
	if got := sidebarJobPrefix(job, at); got != "● agent · 1m00s · " {
		t.Fatal("elapsed prefix does not use draw time", got)
	}
	if got := sidebarTimerSuffix(timer, at); got != " · in 30s" {
		t.Fatal("countdown does not use draw time", got)
	}
	if got := sidebarTimerSuffix(timer, at.Add(time.Minute)); got != " · in 0s" {
		t.Fatal("expired countdown was not clamped", got)
	}
}

func TestSidebarIndependentWheelExpansionAndModalGeometry(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(120, 24)
	b := newSidebar()
	var js []jobs.Snapshot
	var timers []session.TimerView
	var parts []session.TokenPart
	for i := range 40 {
		js = append(js, jobs.Snapshot{ID: fmt.Sprint(i), Label: "job", Kind: "shell"})
		timers = append(timers, session.TimerView{ID: fmt.Sprint(i), Name: "timer"})
		parts = append(parts, session.TokenPart{Name: "part", Tokens: i})
	}
	b.update(session.ContextUsage{Limit: 100000, Parts: parts}, js, timers)
	if b.bounds(120, 24, false) != 86 {
		t.Fatal(b)
	}
	b.draw(s)
	job := &b.sections[1]
	y := job.top + 1
	if consumed, _ := b.mouse(tcell.NewEventMouse(110, y, tcell.WheelDown, 0)); !consumed {
		t.Fatal("wheel not consumed")
	}
	b.draw(s)
	if job.scroll != 3 || b.sections[0].scroll != 0 || b.sections[2].scroll != 0 {
		t.Fatal("cross-list scroll")
	}
	if _, id := b.mouse(tcell.NewEventMouse(110, y, tcell.Button1, 0)); id.jobID != "3" {
		t.Fatal("wrong job clicked", id)
	}
	timer := &b.sections[2]
	b.mouse(tcell.NewEventMouse(110, timer.top+1, tcell.WheelDown, 0))
	b.draw(s)
	if _, action := b.mouse(tcell.NewEventMouse(110, timer.top+1, tcell.Button1, 0)); action.timerID != "3" || action.jobID != "" {
		t.Fatal("wrong scrolled timer clicked", action)
	}
	b.mouse(tcell.NewEventMouse(110, job.top, tcell.Button1, 0))
	if !job.collapsed {
		t.Fatal("not collapsed")
	}
	b.draw(s)
	b.mouse(tcell.NewEventMouse(110, job.top, tcell.Button1, 0))
	b.draw(s)
	if job.scroll != 3 {
		t.Fatal("expansion discarded scroll")
	}
	if b.bounds(80, 24, false) != 80 || b.width != 0 {
		t.Fatal("narrow sidebar stole composer")
	}
	b.overlay = true
	if b.bounds(80, 24, false) != 80 || b.width == 0 {
		t.Fatal("overlay inaccessible")
	}
	b.focus = 0
	b.key(tcell.NewEventKey(tcell.KeyTab, 0, 0))
	if b.focus != 1 {
		t.Fatal("list focus")
	}
	b.key(tcell.NewEventKey(tcell.KeyLeft, 0, 0))
	if !job.collapsed {
		t.Fatal("keyboard collapse")
	}
	b.overlay = false
	b.bounds(120, 24, true)
	if b.width != 0 {
		t.Fatal("fullscreen kept sidebar")
	}
}

func TestSidebarTinyPanes(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	for _, width := range []int{1, 2, 3, 8, 100} {
		for _, height := range []int{1, 2, 3, 8, 20} {
			s.SetSize(width, height)
			b := newSidebar()
			b.workspace = workspace.GitInfo{Cwd: "/research/project", Repo: "/research", Branch: "main"}
			b.overlay = true
			b.bounds(width, height, false)
			b.draw(s)
		}
	}
}
func TestFullscreenHidesComposerAndKeepsAnchor(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(120, 30)
	v := newTranscript()
	for i := range 200 {
		v.append(line{text: fmt.Sprintf("message %d", i), id: int64(i + 1)})
	}
	b := newSidebar()
	draw(s, v, b, false, nil, nil, -1, newComposer(""), 0, nil, nil, "", nil, provider.Selection{})
	v.scroll(-50, 27)
	draw(s, v, b, false, nil, nil, -1, newComposer(""), 0, nil, nil, "", nil, provider.Selection{})
	anchor := v.entryAt(0)
	draw(s, v, b, true, nil, nil, -1, newComposer(""), 0, nil, nil, "", nil, provider.Selection{})
	if v.entryAt(0) != anchor {
		t.Fatal("fullscreen changed anchor")
	}
	_, _, visible := s.GetCursor()
	if visible || b.width != 0 {
		t.Fatal("fullscreen left composer/sidebar")
	}
	draw(s, v, b, true, nil, nil, -1, newComposer("draft"), 0, nil, nil, "", nil, provider.Selection{})
	_, _, visible = s.GetCursor()
	if !visible {
		t.Fatal("typing did not show composer")
	}
	draw(s, v, b, true, nil, nil, -1, newComposer(""), 0, nil, nil, "Working · 0s", nil, provider.Selection{})
	var status string
	for x := range 40 {
		r, _, _, _ := s.GetContent(x, 29)
		status += string(r)
	}
	if !strings.Contains(status, "updates paused") {
		t.Fatal("fullscreen hides running indicator", status)
	}
}

func TestSidebarReportedCountersRemainSeparateFromEstimates(t *testing.T) {
	b := newSidebar()
	cached, reasoning := 0, 70
	u := session.ContextUsage{Model: "new model", Limit: 10000, Input: 345, Reported: &session.ReportedUsage{Model: "old model", Tokens: provider.Usage{InputTokens: 1000, OutputTokens: 100, CachedInputTokens: &cached, ReasoningOutputTokens: &reasoning}}}
	b.update(u, nil, nil)
	rows := strings.Join(b.sections[0].rows, "\n")
	for _, want := range []string{"Last input · reported", "old model", "Input 1000 · cached 0", "Context 3.5%", "Input 345", "345 / 10000 tokens", "Breakdown · estimated"} {
		if !strings.Contains(rows, want) {
			t.Fatalf("missing %q in %s", want, rows)
		}
	}
	u.Reported.Tokens.CachedInputTokens = nil
	b.update(u, nil, nil)
	if !strings.Contains(strings.Join(b.sections[0].rows, "\n"), "cached —") {
		t.Fatal("missing counter shown as zero")
	}
}

func TestSidebarShowsLastInputAndAccumulatedOutput(t *testing.T) {
	b := newSidebar()
	cached, written, reasoning := 600, 100, 2
	lastCached, lastReasoning := 50, 1
	u := session.ContextUsage{Limit: 10000, Input: 200, Reported: &session.ReportedUsage{Tokens: provider.Usage{InputTokens: 200, OutputTokens: 3, CachedInputTokens: &lastCached, ReasoningOutputTokens: &lastReasoning}}, Totals: session.UsageTotals{Requests: 3, ReportedRequests: 2, Tokens: provider.Usage{InputTokens: 1000, OutputTokens: 10, CachedInputTokens: &cached, CacheWriteTokens: &written, ReasoningOutputTokens: &reasoning}}}
	b.update(u, nil, nil)
	rows := strings.Join(b.sections[0].rows, "\n")
	for _, want := range []string{"Last input · reported", "Input 200 · cached 50", "Uncached input 150", "Run output · all agents", "Reported 2/3 requests", "Output 10 · reasoning 2"} {
		if !strings.Contains(rows, want) {
			t.Fatal(want, rows)
		}
	}
	for _, unwanted := range []string{"Input 1000", "cached 600", "Output 3 · reasoning 1", "Ordinary input", "Cache writes"} {
		if strings.Contains(rows, unwanted) {
			t.Fatal("wrong counter scope", unwanted, rows)
		}
	}
	if strings.Count(rows, "Output ") != 1 || strings.Index(rows, "Run output") > strings.Index(rows, "Breakdown") {
		t.Fatal("run output duplicated or hidden below estimates", rows)
	}
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(120, 32)
	b.bounds(120, 32, false)
	b.draw(s)
	var visible strings.Builder
	for y := range 32 {
		visible.WriteString(sidebarScreenText(s, b.left+1, y, b.width-2))
		visible.WriteByte('\n')
	}
	for _, want := range []string{"Input 200 · cached 50", "Output 10 · reasoning 2"} {
		if !strings.Contains(visible.String(), want) {
			t.Fatal("counter not visible in headless terminal", want, visible.String())
		}
	}
	u.Totals.Tokens.ReasoningOutputTokens = nil
	b.update(u, nil, nil)
	if rows = strings.Join(b.sections[0].rows, "\n"); !strings.Contains(rows, "Output 10 · reasoning —") {
		t.Fatal("missing reasoning total shown as zero or last response", rows)
	}
}

func TestSidebarUsesReportedInputOnlyForMatchingRequest(t *testing.T) {
	b := newSidebar()
	u := session.ContextUsage{Model: "model", RequestID: 3, Limit: 10000, Input: 900, Reserved: 100, Reported: &session.ReportedUsage{Model: "model", RequestID: 3, Tokens: provider.Usage{InputTokens: 2000}}}
	b.update(u, nil, nil)
	rows := strings.Join(b.sections[0].rows, "\n")
	if !strings.Contains(rows, "Context 21.0%") || !strings.Contains(rows, "2100 / 10000") || !strings.Contains(rows, "reported input + reserve") {
		t.Fatal(rows)
	}
	u.RequestID = 4
	b.update(u, nil, nil)
	rows = strings.Join(b.sections[0].rows, "\n")
	if !strings.Contains(rows, "Context 10.0%") || strings.Contains(rows, "reported input + reserve") {
		t.Fatal(rows)
	}
}
