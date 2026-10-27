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

// drawInputCursor overlays a native cursor without consuming a text cell.
// Coordinates are relative to the input's visible content rectangle.
func drawInputCursor(s tcell.Screen, left, top, width, height int, cursor *cellPosition) {
	if cursor == nil || cursor.x < 0 || cursor.x >= width || cursor.y < 0 || cursor.y >= height {
		s.HideCursor()
		return
	}
	s.ShowCursor(left+cursor.x, top+cursor.y)
}

// drawState contains the inputs for one frame; referenced UI state remains shared.
type drawState struct {
	view       *transcript
	sidebar    *sidebar
	fullscreen bool
	preview    *imagePreview
	renderer   *imageRenderer
	focused    int      // Transcript line index; -1 means no focus (zero is valid).
	draft      composer // Value copy: layout and scrolling updates stay local to the frame.
	attached   int
	queue      []contextbuild.Input
	steers     []string
	indicator  string
	window     *Window
	question   *questionDialog
	selection  llm.Selection
}

func draw(s tcell.Screen, frame drawState) error {
	s.Clear()
	w, h := s.Size()
	paneWidth := frame.sidebar.bounds(w, h, frame.fullscreen)
	w = paneWidth
	style := tcell.StyleDefault.Foreground(tcell.GetColor(render.TextColor))
	var geometry inputGeometry
	var questionGeometry questionInputGeometry
	var queuedLines, height int
	if frame.question != nil {
		questionGeometry = layoutQuestionInput(w, h, frame.fullscreen, len(frame.queue)+len(frame.steers))
		queuedLines, height = questionGeometry.queuedRows, questionGeometry.transcriptHeight
		frame.question.reveal(questionGeometry.bodyWidth, questionGeometry.bodyHeight)
	} else {
		geometry = layoutInput(&frame.draft, w, h, frame.fullscreen, len(frame.queue)+len(frame.steers))
		queuedLines, height = geometry.queuedRows, geometry.transcriptHeight
	}
	columns := max(1, w-1)
	if frame.fullscreen {
		columns = max(1, w)
	}
	all := frame.view.viewport(columns, height)
	start := frame.view.firstLine
	if frame.renderer != nil {
		assets := all
		if frame.window != nil && frame.question == nil && frame.preview == nil && frame.window.Markdown {
			if frame.window.renderer != frame.renderer {
				frame.window.renderer = frame.renderer
				frame.window.cachedLines = nil
			}
			// Prepare the popup viewport before requesting assets. The shared
			// worker and memory bound cover both visible conversation and popup.
			fullWidth, fullHeight := s.Size()
			innerWidth, bodyHeight := windowContentSize(fullWidth, fullHeight, frame.window)
			rows := frame.window.Lines(innerWidth, bodyHeight)
			assets = append(append([]line(nil), all...), make([]line, len(rows))...)
			for i := range rows {
				row := frame.window.assets[frame.window.Scroll+i]
				assets[len(all)+i] = line{text: row.text, assets: row.assets}
			}
		}
		if err := frame.renderer.ensure(assets); err != nil {
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
		if frame.focused >= 0 && frame.focused < len(frame.view.lines) && all[i].id != 0 && all[i].id == frame.view.lines[frame.focused].id {
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
	if !frame.fullscreen {
		drawScrollBar(s, w-1, 0, height, frame.view.total, start, style.Foreground(tcell.GetColor(render.MutedColor)))
	}
	status := frame.selection.Model.ID + " · " + frame.selection.Variant + " · /help · Ctrl+X F full · Ctrl+X M model"
	if frame.indicator != "" {
		status = frame.indicator
	}
	if !frame.fullscreen {
		if (frame.question == nil && h >= 3) || (frame.question != nil && h >= 6) {
			put(s, 0, height, w, status, style.Foreground(tcell.GetColor(render.CyanColor)))
		}
		for i := range queuedLines {
			label := "Queued · "
			var source string
			if i < len(frame.steers) {
				label = "Steer · "
				source = frame.steers[i]
			} else {
				source = frame.queue[i-len(frame.steers)].Text
			}
			text := pendingInputPreview(label, source, w)
			put(s, 0, height+1+i, w, strings.Repeat(" ", max(0, w)), style.Background(tcell.GetColor(render.HumanColor)))
			putStyled(s, 0, height+1+i, w, text, style.Background(tcell.GetColor(render.HumanColor)))
		}
	}
	if frame.question == nil {
		for i, row := range geometry.input.rows {
			if i == 0 {
				put(s, 0, geometry.inputY+i, w, "> ", style)
			}
			put(s, 2, geometry.inputY+i, max(0, w-2), row.text, style)
		}
	}
	progress := scrollIndicator(frame.view.total, height, start, w)
	if len(frame.view.blocks) > len(frame.view.recent) {
		progress = "~" + progress
	}
	if frame.fullscreen {
		progress = ""
		put(s, 0, h-1, w, "Copy mode · updates paused · Esc returns", style.Foreground(tcell.GetColor(render.CyanColor)))
	}
	if frame.attached > 0 && frame.question == nil {
		put(s, 0, h-1, max(0, w-runewidth.StringWidth(progress)-1), fmt.Sprintf("%d attachments", frame.attached), style)
	}
	if frame.question != nil && h > 0 {
		put(s, 0, h-1, max(0, w-runewidth.StringWidth(progress)-1), "Ctrl+U/D context · PgUp/PgDn question · Tab tabs", style.Foreground(tcell.GetColor(render.CyanColor)))
	}
	put(s, max(0, w-runewidth.StringWidth(progress)), h-1, w, progress, style.Foreground(tcell.GetColor(render.MutedColor)))
	if frame.question != nil {
		drawQuestionInput(s, frame.question, questionGeometry, style)
		// The narrow-pane sidebar is an overlay; do not expose a cursor on
		// top of it when it covers the custom answer's insertion point.
		if frame.sidebar.overlay && frame.sidebar.width > 0 && frame.question.Window.body.cursor != nil && frame.question.Window.body.cursor.x >= frame.sidebar.left {
			s.HideCursor()
		}
	}
	frame.sidebar.draw(s)
	if frame.question == nil && frame.window == nil && frame.preview == nil && frame.draft.completion != nil {
		frame.draft.completion.draw(s, geometry.inputY, w)
	}
	if frame.preview != nil && frame.renderer != nil {
		s.HideCursor()
		if err := frame.preview.draw(s, frame.renderer.graphics); err != nil {
			drawWindow(s, &Window{Title: "Image preview", Text: "Image preview failed: " + err.Error()})
		}
	} else if frame.question == nil && frame.window != nil {
		s.HideCursor()
		drawWindow(s, frame.window)
	} else if frame.question == nil {
		drawInputCursor(s, 2, geometry.inputY, max(0, w-2), len(geometry.input.rows),
			&cellPosition{geometry.input.cursorX, geometry.input.cursorY})
	}
	s.Show()
	return nil
}

// drawQuestionInput uses Window only as the form's body cache and viewport;
// unlike drawWindow, it never covers the transcript or sidebar with a popup.
func drawQuestionInput(s tcell.Screen, question *questionDialog, geometry questionInputGeometry, style tcell.Style) {
	window := &question.Window
	if geometry.bodyY > geometry.headerY {
		header := window.HeaderLines(geometry.bodyWidth, 1)
		if len(header) > 0 {
			put(s, 0, geometry.headerY, geometry.bodyWidth, header[0], style.Foreground(tcell.GetColor(render.CyanColor)).Bold(true))
		}
	}
	rows := window.Lines(geometry.bodyWidth, geometry.bodyHeight)
	for i, row := range rows {
		put(s, 0, geometry.bodyY+i, geometry.bodyWidth, row, style)
	}
	drawScrollBar(s, geometry.bodyWidth, geometry.bodyY, geometry.bodyHeight, len(window.cachedLines), window.Scroll, style.Foreground(tcell.GetColor(render.MutedColor)))
	var cursor *cellPosition
	if window.body.cursor != nil {
		cursor = &cellPosition{window.body.cursor.x, window.body.cursor.y - window.Scroll}
	}
	drawInputCursor(s, 0, geometry.bodyY, geometry.bodyWidth, geometry.bodyHeight, cursor)
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
	hint := window.Hint
	if hint == "" {
		hint = "Esc closes"
	}
	title += render.Clean(" · " + hint)
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
		if (window.Markdown || window.Styled || window.Structured) && i >= len(header) {
			putStyled(s, left+1, top+1+i, innerWidth, text, style)
		} else {
			put(s, left+1, top+1+i, innerWidth, text, style)
		}
	}
	progress := window.Progress(bodyHeight, width-2)
	put(s, left+width-1-runewidth.StringWidth(progress), top+height-1, width-2, progress, style.Bold(true))
	drawScrollBar(s, left+width-1, top+1+len(header), bodyHeight, len(window.cachedLines), window.Scroll, style)
	var cursor *cellPosition
	if window.body.cursor != nil {
		cursor = &cellPosition{window.body.cursor.x, window.body.cursor.y - window.Scroll}
	}
	drawInputCursor(s, left+1, top+1+len(header), innerWidth, bodyHeight, cursor)
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
