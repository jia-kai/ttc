package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v3"
	"github.com/mattn/go-runewidth"
	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/render"
)

func put(s tcell.Screen, x, y, width int, text string, style tcell.Style) {
	for len(text) > 0 && width > 0 {
		cluster, cells := ansi.FirstGraphemeCluster(text, ansi.GraphemeWidth)
		text = text[len(cluster):]
		if cells > width {
			return
		}
		s.Put(x, y, cluster, style)
		x += cells
		width -= cells
	}
}
func draw(s tcell.Screen, view *transcript, sidebar *sidebar, fullscreen bool, preview *imagePreview, renderer *imageRenderer, focused int, draft composer, attached int, queue []contextbuild.Input, steers []string, indicator string, window *Window, selection llm.Selection) error {
	s.Clear()
	w, h := s.Size()
	paneWidth := sidebar.bounds(w, h, fullscreen)
	w = paneWidth
	style := tcell.StyleDefault.Foreground(tcell.GetColor(render.TextColor))
	geometry := layoutInput(&draft, w, h, fullscreen, len(queue)+len(steers))
	queuedLines, height := geometry.queuedRows, geometry.transcriptHeight
	columns := max(1, w-1)
	if fullscreen {
		columns = max(1, w)
	}
	all := view.viewport(columns, height)
	start := view.firstLine
	if renderer != nil {
		assets := all
		if window != nil && preview == nil && window.Markdown {
			if window.renderer != renderer {
				window.renderer = renderer
				window.cachedLines = nil
			}
			// Prepare the popup viewport before requesting assets. The shared
			// worker and memory bound cover both visible conversation and popup.
			fullWidth, fullHeight := s.Size()
			innerWidth, bodyHeight := windowContentSize(fullWidth, fullHeight, window)
			rows := window.Lines(innerWidth, bodyHeight)
			assets = append(append([]line(nil), all...), make([]line, len(rows))...)
			for i := range rows {
				row := window.assets[window.Scroll+i]
				assets[len(all)+i] = line{text: row.text, assets: row.assets}
			}
		}
		if err := renderer.ensure(assets); err != nil {
			return err
		}
	}
	for y, i := 0, 0; y < height && i < len(all); y, i = y+1, i+1 {
		st := style
		if all[i].boundary {
			st = st.Foreground(tcell.GetColor(render.BorderColor))
		}
		if all[i].human && !all[i].boundary {
			st = st.Background(tcell.GetColor(render.HumanColor))
			put(s, 0, y, columns, strings.Repeat(" ", columns), st)
		}
		if all[i].system {
			st = st.Foreground(tcell.GetColor(render.LavenderColor))
		}
		if focused >= 0 && focused < len(view.lines) && all[i].id != 0 && all[i].id == view.lines[focused].id {
			st = st.Reverse(true)
		}
		if strings.ContainsRune(all[i].text, '\U0010eeee') {
			st = st.Reverse(false)
		}
		if all[i].markdown || all[i].styled {
			putStyled(s, 0, y, columns, all[i].text, st)
		} else {
			put(s, 0, y, columns, all[i].text, st)
		}
	}
	if !fullscreen {
		drawScrollBar(s, w-1, 0, height, view.total, start, style.Foreground(tcell.GetColor(render.MutedColor)))
	}
	status := selection.Model.ID + " · " + selection.Variant + " · /help · Ctrl+X F full · Ctrl+X M model"
	if indicator != "" {
		status = indicator
	}
	if !fullscreen {
		if h >= 3 {
			put(s, 0, height, w, status, style.Foreground(tcell.GetColor(render.CyanColor)))
		}
		for i := range queuedLines {
			label := "Queued · "
			var source string
			if i < len(steers) {
				label = "Steer · "
				source = steers[i]
			} else {
				source = queue[i-len(steers)].Text
			}
			text := pendingInputPreview(label, source, w)
			put(s, 0, height+1+i, w, strings.Repeat(" ", max(0, w)), style.Background(tcell.GetColor(render.HumanColor)))
			putStyled(s, 0, height+1+i, w, text, style.Background(tcell.GetColor(render.HumanColor)))
		}
	}
	for i, row := range geometry.input.rows {
		if i == 0 {
			put(s, 0, geometry.inputY+i, w, "> ", style)
		}
		put(s, 2, geometry.inputY+i, max(0, w-2), row.text, style)
	}
	progress := scrollIndicator(view.total, height, start, w)
	if len(view.blocks) > len(view.recent) {
		progress = "~" + progress
	}
	if fullscreen {
		progress = ""
		put(s, 0, h-1, w, "Copy mode · updates paused · Esc returns", style.Foreground(tcell.GetColor(render.CyanColor)))
	}
	if attached > 0 {
		put(s, 0, h-1, max(0, w-runewidth.StringWidth(progress)-1), fmt.Sprintf("%d attachments", attached), style)
	}
	put(s, max(0, w-runewidth.StringWidth(progress)), h-1, w, progress, style.Foreground(tcell.GetColor(render.MutedColor)))
	sidebar.draw(s)
	if window == nil && preview == nil && draft.completion != nil {
		draft.completion.draw(s, geometry.inputY, w)
	}
	if preview != nil && renderer != nil {
		s.HideCursor()
		if err := preview.draw(s, renderer.graphics); err != nil {
			return err
		}
	} else if window != nil {
		s.HideCursor()
		drawWindow(s, window)
	} else if w > 0 && geometry.input.cursorY >= 0 {
		s.ShowCursor(min(2+geometry.input.cursorX, w-1), geometry.inputY+geometry.input.cursorY)
	} else {
		s.HideCursor()
	}
	s.Show()
	return nil
}

func drawScrollBar(s tcell.Screen, x, top, height, total, offset int, style tcell.Style) {
	if x < 0 || height <= 0 {
		return
	}
	thumb := height
	position := 0
	if total > height {
		thumb = max(1, height*height/total)
		position = min(max(0, offset), total-height) * (height - thumb) / (total - height)
	}
	for y := range height {
		r := '│'
		if y >= position && y < position+thumb {
			r = '█'
		}
		s.Put(x, top+y, string(r), style)
	}
}

func drawWindowFrame(s tcell.Screen, window *Window) {
	w, h := s.Size()
	left, top, width, height := windowBounds(w, h)
	if width < 2 || height < 2 {
		return
	}
	style := tcell.StyleDefault.Background(tcell.GetColor(render.SurfaceColor)).Foreground(tcell.GetColor(render.TextColor))
	if window.System {
		style = style.Foreground(tcell.GetColor(render.LavenderColor))
	}
	for y := range height {
		put(s, left, top+y, width, strings.Repeat(" ", width), style)
	}
	for x := 1; x < width-1; x++ {
		s.Put(left+x, top, "─", style)
		s.Put(left+x, top+height-1, "─", style)
	}
	for y := 1; y < height-1; y++ {
		s.Put(left, top+y, "│", style)
		s.Put(left+width-1, top+y, "│", style)
	}
	s.Put(left, top, "┌", style)
	s.Put(left+width-1, top, "┐", style)
	s.Put(left, top+height-1, "└", style)
	s.Put(left+width-1, top+height-1, "┘", style)
	title := render.Clean(window.Title)
	if !window.HideHint {
		hint := window.Hint
		if hint == "" {
			hint = "Esc closes"
		}
		title += render.Clean(" · " + hint)
	}
	if window.subagentName != "" {
		title = render.SubagentBadge(window.actor, window.subagentName, max(1, (width-2)/2), true) + " " + title
	}
	putStyled(s, left+1, top, width-2, title, style.Bold(true))
}
func drawWindow(s tcell.Screen, window *Window) {
	w, h := s.Size()
	left, top, width, height := windowBounds(w, h)
	if width < 2 || height < 2 {
		return
	}
	style := tcell.StyleDefault.Background(tcell.GetColor(render.SurfaceColor)).Foreground(tcell.GetColor(render.TextColor))
	if window.System {
		style = style.Foreground(tcell.GetColor(render.LavenderColor))
	}
	drawWindowFrame(s, window)
	innerWidth, bodyHeight := windowContentSize(w, h, window)
	header := window.HeaderLines(innerWidth, max(0, height-3))
	view := window.Lines(innerWidth, bodyHeight)
	for i, text := range append(header, view...) {
		if (window.Markdown || window.Styled) && i >= len(header) {
			putStyled(s, left+1, top+1+i, innerWidth, text, style)
		} else {
			put(s, left+1, top+1+i, innerWidth, text, style)
		}
	}
	progress := window.Progress(bodyHeight, width-2)
	put(s, left+width-1-runewidth.StringWidth(progress), top+height-1, width-2, progress, style.Bold(true))
	drawScrollBar(s, left+width-1, top+1+len(header), bodyHeight, len(window.cachedLines), window.Scroll, style)
}

// windowContentSize accounts for the border and fixed headers in every caller.
func windowContentSize(w, h int, window *Window) (width, height int) {
	_, _, outerWidth, outerHeight := windowBounds(w, h)
	width, height = max(0, outerWidth-2), max(0, outerHeight-2)
	return width, height - len(window.HeaderLines(width, max(0, height-1)))
}

func windowBounds(w, h int) (left, top, width, height int) {
	left, top, width, height = 2, 1, w-4, h-2
	if width < 5 || height < 3 {
		left, top, width, height = 0, 0, w, h
	}
	return
}
