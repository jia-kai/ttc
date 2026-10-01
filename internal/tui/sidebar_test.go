package tui

import (
	"fmt"
	"github.com/gdamore/tcell/v2"
	"scicode/internal/jobs"
	"scicode/internal/provider"
	"scicode/internal/session"
	"strings"
	"testing"
	"time"
)

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
		timers = append(timers, session.TimerView{Name: "timer"})
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
			b.workspace = workspaceInfo{cwd: "/research/project", repo: "/research", branch: "main"}
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
	draw(s, v, b, false, nil, nil, -1, newComposer(""), 0, nil, false, time.Time{}, nil, provider.Selection{})
	v.scroll(-50, 27)
	draw(s, v, b, false, nil, nil, -1, newComposer(""), 0, nil, false, time.Time{}, nil, provider.Selection{})
	anchor := v.entryAt(0)
	draw(s, v, b, true, nil, nil, -1, newComposer(""), 0, nil, false, time.Time{}, nil, provider.Selection{})
	if v.entryAt(0) != anchor {
		t.Fatal("fullscreen changed anchor")
	}
	_, _, visible := s.GetCursor()
	if visible || b.width != 0 {
		t.Fatal("fullscreen left composer/sidebar")
	}
	draw(s, v, b, true, nil, nil, -1, newComposer("draft"), 0, nil, false, time.Time{}, nil, provider.Selection{})
	_, _, visible = s.GetCursor()
	if !visible {
		t.Fatal("typing did not show composer")
	}
	draw(s, v, b, true, nil, nil, -1, newComposer(""), 0, nil, true, time.Now(), nil, provider.Selection{})
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
	for _, want := range []string{"Last response · reported", "old model", "Input 1000 · cached 0", "Output 100 · reasoning 70", "Context 3.5%", "Input 345", "345 / 10000 tokens", "Breakdown · estimated"} {
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

func TestSidebarRunTotalsIncludeCacheReadsAndWrites(t *testing.T) {
	b := newSidebar()
	cached, written, reasoning := 600, 100, 2
	u := session.ContextUsage{Totals: session.UsageTotals{Requests: 3, ReportedRequests: 2, Tokens: provider.Usage{InputTokens: 1000, OutputTokens: 10, CachedInputTokens: &cached, CacheWriteTokens: &written, ReasoningOutputTokens: &reasoning}}}
	b.update(u, nil, nil)
	rows := strings.Join(b.sections[0].rows, "\n")
	for _, want := range []string{"Run totals · all agents", "Reported 2/3 requests", "cached 600", "Cache writes 100", "Ordinary input 300", "Output 10 · reasoning 2"} {
		if !strings.Contains(rows, want) {
			t.Fatal(want, rows)
		}
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
