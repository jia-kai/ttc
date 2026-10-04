package tui

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"path/filepath"
	"strings"
	"sync"

	"ttc/internal/assets"
	"ttc/internal/graphics"
	"ttc/internal/render"
	"ttc/internal/session"

	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
)

// displayRow shares the exact rendered cells and assets between draw/hit testing.
type displayRow struct {
	styled bool // Already-rendered ANSI decorations, independent of source markup.
	text   string
	assets []placedImage
	source int
}
type placedImage struct {
	key, path, tex string
	columns, rows  int
}
type renderTask struct {
	key, path, tex string
	pixels         int
	source         bool
	ctx            context.Context
}
type renderReply struct {
	key     string
	pixels  image.Image
	err     error
	source  bool
	math    bool // Formula raster; never a thumbnail or full-resolution source.
	backend string
}

var errViewportImageMemory = errors.New("viewport image memory limit exceeded")

// imageRenderer owns one cancelable worker and only queues assets in visible rows.
// Decoded thumbnails and the formula LRU share a 32 MiB budget. Kitty uploads
// remain viewport-only, and nonvisible worker results are discarded.
type imageRenderer struct {
	graphics              *graphics.Kitty
	cellWidth, cellHeight int
	cellFallback          bool // A warning has been shown for this fallback interval.
	tasks                 chan renderTask
	sources               chan renderTask
	results               chan renderReply
	pending               map[string]context.CancelFunc
	ready                 map[string]renderReply
	visible               map[string]bool
	imageBytes            int // Visible thumbnail bytes; formula bytes belong to mathCache.
	mathCache             *mathRasterCache
	ctx                   context.Context
	cancel                context.CancelFunc
	wg                    sync.WaitGroup
	backend               string
	backendError          error
	revision              uint64 // Invalidates visible Markdown layouts when a reply arrives.
	pendingClick          func(string) bool
}

func newImageRenderer(ctx context.Context, g *graphics.Kitty) (*imageRenderer, error) {
	cache, err := assets.Default()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	// Hand decoded images directly to the UI; supersampled replies must not
	// accumulate in a buffered queue outside the viewport's memory accounting.
	r := &imageRenderer{graphics: g, cellWidth: 8, cellHeight: 16, tasks: make(chan renderTask, 32), sources: make(chan renderTask, 1), results: make(chan renderReply), pending: map[string]context.CancelFunc{}, ready: map[string]renderReply{}, visible: map[string]bool{}, ctx: ctx, cancel: cancel}
	r.wg.Add(1)
	go r.work(cache)
	return r, nil
}

// updateCellDimensions checks measurements before division, reports entry into
// fallback once, and synchronizes placement identity with the effective size.
func (r *imageRenderer) updateCellDimensions(size tcell.WindowSize) (changed bool, warning string) {
	cw, ch := 0, 0
	if size.Width > 0 && size.Height > 0 && size.PixelWidth > 0 && size.PixelHeight > 0 {
		cw, ch = size.CellDimensions()
	}
	fallback := cw < 1 || ch < 1
	if fallback {
		cw, ch = 8, 16
		if !r.cellFallback {
			warning = "Warning: using estimated 8×16 pixel cells.\nTerminal pixel dimensions unavailable; graphics sizing may be inaccurate."
		}
	}
	r.cellFallback = fallback
	changed = cw != r.cellWidth || ch != r.cellHeight
	if changed {
		r.revision++
	}
	r.cellWidth, r.cellHeight = cw, ch
	r.graphics.CellWidth, r.graphics.CellHeight = cw, ch
	return changed, warning
}
func (r *imageRenderer) work(cache *assets.Cache) {
	defer r.wg.Done()
	type initializedMath struct {
		renderer *assets.MathRenderer
		err      error
	}
	initializing := make(chan initializedMath, 1)
	go func(result chan<- initializedMath) {
		renderer, err := assets.NewMathRenderer(r.ctx)
		result <- initializedMath{renderer, err}
	}(initializing)
	var mathRenderer *assets.MathRenderer
	defer func() {
		// Join initialization even when the frontend exits before receiving it.
		if initializing != nil {
			mathRenderer = (<-initializing).renderer
		}
		if mathRenderer != nil {
			mathRenderer.Close()
		}
	}()
	acceptMath := func(initialized initializedMath) bool {
		mathRenderer = initialized.renderer
		initializing = nil
		version := "unavailable"
		if mathRenderer != nil {
			version = mathRenderer.Version()
		}
		select {
		case r.results <- renderReply{backend: version, err: initialized.err}:
			return true
		case <-r.ctx.Done():
			return false
		}
	}
	for {
		select {
		case initialized := <-initializing:
			if !acceptMath(initialized) {
				return
			}
		default:
		}
		var task renderTask
		select {
		case task = <-r.sources:
		default:
			select {
			case initialized := <-initializing:
				if !acceptMath(initialized) {
					return
				}
				continue
			case task = <-r.sources:
			case task = <-r.tasks:
			case <-r.ctx.Done():
				return
			}
		}
		if task.ctx == nil {
			task.ctx = r.ctx
		}
		var pixels image.Image
		var err error
		if err = task.ctx.Err(); err == nil {
			if task.source {
				_, pixels, err = assets.Read(task.path)
			} else {
				pixels, err = cache.Get(task.ctx, task.key, func(ctx context.Context) (image.Image, error) {
					if task.path != "" {
						_, m, err := assets.Read(task.path)
						if err != nil {
							return nil, err
						}
						return assets.Resize(m, 512, 320), nil
					}
					if mathRenderer == nil {
						return nil, fmt.Errorf("MathJax backend unavailable")
					}
					m, err := mathRenderer.Render(ctx, task.tex, task.pixels)
					if err != nil {
						return nil, err
					}
					return m, nil
				})
			}
		}
		select {
		case r.results <- renderReply{key: task.key, pixels: pixels, err: err, source: task.source, math: task.tex != ""}:
		case <-r.ctx.Done():
			return
		}
	}
}
func (r *imageRenderer) loadSource(v session.ImageSnapshot) error {
	select {
	case r.sources <- renderTask{key: v.ID, path: v.Snapshot, source: true}:
		return nil
	default:
		return fmt.Errorf("image preview is already loading")
	}
}
func (r *imageRenderer) close() error { r.cancel(); r.wg.Wait(); return r.graphics.Close() }
func imageBytes(m image.Image) int {
	if m == nil {
		return 0
	}
	return m.Bounds().Dx() * m.Bounds().Dy() * 4
}
func (r *imageRenderer) accept(reply renderReply) bool {
	if reply.backend != "" {
		r.backend, r.backendError = reply.backend, reply.err
		r.revision++
		return true
	}
	if reply.source {
		return false
	}
	if cancel := r.pending[reply.key]; cancel != nil {
		cancel()
		delete(r.pending, reply.key)
	}
	if !r.visible[reply.key] || errors.Is(reply.err, context.Canceled) {
		return false
	}
	old := r.ready[reply.key]
	if !old.math {
		r.imageBytes -= imageBytes(old.pixels)
	}
	if reply.math && reply.pixels != nil && reply.err == nil {
		if r.mathCache == nil {
			r.mathCache = &mathRasterCache{}
		}
		ok, removed := r.mathCache.put(reply.key, reply.pixels, decodedImageLimit-r.imageBytes)
		r.evictMath(removed)
		if !ok {
			reply.pixels = nil
			reply.err = errViewportImageMemory
		}
	} else if !reply.math && r.imageBytes+imageBytes(reply.pixels) > decodedImageLimit {
		reply.pixels = nil
		reply.err = errViewportImageMemory
	}
	r.ready[reply.key] = reply
	if !reply.math {
		r.imageBytes += imageBytes(reply.pixels)
		r.trimMath(decodedImageLimit - r.imageBytes)
	}
	r.revision++
	return true
}
func (r *imageRenderer) request(p placedImage) {
	if p.tex != "" {
		r.cachedMath(p.key)
	}
	if _, ok := r.ready[p.key]; ok {
		return
	}
	if r.pending[p.key] != nil {
		return
	}
	if p.tex != "" && (r.backend == "" || r.backendError != nil) {
		return
	}
	ctx, cancel := context.WithCancel(r.ctx)
	task := renderTask{key: p.key, path: p.path, tex: p.tex, pixels: max(12, r.cellHeight-2), ctx: ctx}
	select {
	case r.tasks <- task:
		r.pending[p.key] = cancel
	default:
		cancel()
	}
}
func rowsOf(text []string) []displayRow {
	out := make([]displayRow, len(text))
	for i, v := range text {
		out[i].text = v
	}
	return out
}
func (r *imageRenderer) layout(v line, width int) []displayRow {
	if v.image != nil {
		p := placedImage{key: assets.Key("thumbnail-v1", v.image.Snapshot), path: v.image.Snapshot}
		if r.pendingClick != nil && r.pendingClick(v.image.ID) {
			v.text = "**click pending** · " + v.text
		}
		out := rowsOf(layoutText(v, width))
		reply, ok := r.ready[p.key]
		if ok && reply.err != nil {
			return append(out, displayRow{text: "Image unavailable: " + render.Clean(reply.err.Error()), assets: []placedImage{p}})
		}
		if reply.pixels == nil {
			return append(out, displayRow{text: "Loading image…", assets: []placedImage{p}})
		}
		p.columns, p.rows = graphics.Geometry(reply.pixels, r.cellWidth, r.cellHeight, min(width, 48), 10)
		grid, _ := r.graphics.Rows(p.key, p.columns, p.rows)
		for _, row := range grid {
			out = append(out, displayRow{text: row, assets: []placedImage{p}})
		}
		return out
	}
	if !v.markdown || v.brief {
		return rowsOf(layoutText(v, width))
	}
	type replacement struct {
		asset  placedImage
		row    string
		cursor int
	}
	replacements := map[rune]*replacement{}
	marker := rune(0xe000)
	source := render.Math(v.text, func(tex string, block bool) string {
		p := placedImage{key: assets.Key("mathjax-hires-v1", r.backend, tex, fmt.Sprint(r.cellHeight), fmt.Sprint(assets.MathRasterScale)), tex: tex}
		reply := r.cachedMath(p.key)
		var grid []string
		if reply.pixels != nil {
			// Measure the supersampled bitmap in logical terminal pixels. The
			// upload filters it to this cell grid's measured pixel dimensions.
			p.columns, p.rows = graphics.Geometry(reply.pixels, r.cellWidth*assets.MathRasterScale, r.cellHeight*assets.MathRasterScale, max(1, min(width-6, 200)), 12)
			if p.rows > 1 || p.columns > max(8, width/2) {
				block = true
			}
			grid, _ = r.graphics.Rows(p.key, p.columns, p.rows)
		} else {
			literal := strings.Join(strings.Fields(render.Clean(tex)), " ")
			if reply.err != nil || r.backendError != nil {
				literal = "[math unavailable] " + literal
			}
			p.columns = max(1, min(max(1, width-6), runewidth.StringWidth(literal)))
			p.rows = 1
			grid = []string{ansi.Truncate(literal, p.columns, "…")}
		}
		tokens := make([]string, len(grid))
		for y, row := range grid {
			for strings.ContainsRune(v.text, marker) {
				marker++
			}
			tokens[y] = strings.Repeat(string(marker), p.columns)
			replacements[marker] = &replacement{asset: p, row: row}
			marker++
		}
		text := strings.Join(tokens, "  \n")
		if block {
			return "\n\n" + text + "\n\n"
		}
		return text
	})
	v.text = source
	out := rowsOf(layoutText(v, width))
	for i := range out {
		for rune, replacement := range replacements {
			token := string(rune)
			for {
				start := strings.Index(out[i].text, token)
				if start < 0 {
					break
				}
				end := start
				for strings.HasPrefix(out[i].text[end:], token) {
					end += len(token)
				}
				cells := (end - start) / len(token)
				fragment := ansi.Cut(replacement.row, replacement.cursor, replacement.cursor+cells)
				replacement.cursor += cells
				out[i].text = out[i].text[:start] + fragment + out[i].text[end:]
				out[i].assets = append(out[i].assets, replacement.asset)
			}
		}
	}
	return out
}
func (r *imageRenderer) ensure(rows []line) error {
	seen := map[string]bool{}
	placed := map[string]bool{}
	for _, v := range rows {
		for _, p := range v.assets {
			seen[p.key] = true
			r.request(p)
			if m := r.ready[p.key].pixels; m != nil && strings.ContainsRune(v.text, '\U0010eeee') {
				id := fmt.Sprintf("%s:%dx%d", p.key, p.columns, p.rows)
				if !placed[id] {
					scale := 1
					if p.tex != "" {
						scale = assets.MathRasterScale
					}
					if _, err := r.graphics.Place(p.key, m, p.columns, p.rows, scale); err != nil {
						return err
					}
					placed[id] = true
				}
			}
		}
	}
	r.visible = seen
	for key, cancel := range r.pending {
		if !seen[key] {
			cancel()
		}
	}
	for key, reply := range r.ready {
		if !seen[key] {
			if !reply.math {
				r.imageBytes -= imageBytes(reply.pixels)
			}
			delete(r.ready, key)
		}
	}
	return nil
}

// imagePreview maps the bordered canvas to original source coordinates. Panning
// is in source pixels; zoom is relative to aspect-preserving fit. Input is cell
// precision, including measured letterboxing. A click selects; OK confirms.
type imagePreview struct {
	Window                   Window
	snapshot                 session.ImageSnapshot
	pixels                   image.Image
	zoom, panX, panY         float64
	point                    *[2]int
	left, top, width, height int
	cellWidth, cellHeight    int
	scale, originX, originY  float64
	pending                  bool
	canvas                   image.Image
	canvasKey                string
	detailText               string
	details                  *Window
}

func newImagePreview(v session.ImageSnapshot, m image.Image, pending bool, cw, ch int) *imagePreview {
	return &imagePreview{Window: Window{Title: "Image · " + filepath.Base(v.Path), Hint: "h/j/k/l pan · +/- zoom · 0 fit · i details · Esc cancel"}, snapshot: v, pixels: m, zoom: 1, pending: pending, cellWidth: cw, cellHeight: ch}
}
func (p *imagePreview) geometry(w, h int) {
	left, top, width, height := windowBounds(w, h)
	p.left, p.top = left+1, top+1
	p.width, p.height = min(200, max(1, width-2)), min(100, max(1, height-4))
	pw, ph := float64(p.width*p.cellWidth), float64(p.height*p.cellHeight)
	p.scale = min(pw/float64(p.snapshot.Width), ph/float64(p.snapshot.Height)) * p.zoom
	p.originX = (pw-float64(p.snapshot.Width)*p.scale)/2 - p.panX*p.scale
	p.originY = (ph-float64(p.snapshot.Height)*p.scale)/2 - p.panY*p.scale
}
func (p *imagePreview) coordinate(x, y int) *[2]int {
	if x < p.left || x >= p.left+p.width || y < p.top || y >= p.top+p.height {
		return nil
	}
	sx := (float64((x-p.left)*p.cellWidth) + float64(p.cellWidth)/2 - p.originX) / p.scale
	sy := (float64((y-p.top)*p.cellHeight) + float64(p.cellHeight)/2 - p.originY) / p.scale
	if sx < 0 || sy < 0 || sx >= float64(p.snapshot.Width) || sy >= float64(p.snapshot.Height) {
		return nil
	}
	point := [2]int{int(sx), int(sy)}
	return &point
}
func (p *imagePreview) key(ev *tcell.EventKey) (close, confirm bool) {
	if p.details != nil {
		if ev.Key() == tcell.KeyEscape {
			p.details = nil
		} else {
			p.details.Key(ev, p.height)
		}
		return false, false
	}
	switch ev.Key() {
	case tcell.KeyEscape:
		return true, false
	case tcell.KeyEnter:
		if p.pending && p.point != nil {
			return true, true
		}
	case tcell.KeyRune:
		switch ev.Rune() {
		case 'i':
			p.details = &Window{Title: "Image tool details", Text: p.detailText, Markdown: true, actor: p.Window.actor, subagentName: p.Window.subagentName}
		case 'h':
			p.panX -= float64(p.cellWidth) * 4 / p.scale
		case 'l':
			p.panX += float64(p.cellWidth) * 4 / p.scale
		case 'k':
			p.panY -= float64(p.cellHeight) * 2 / p.scale
		case 'j':
			p.panY += float64(p.cellHeight) * 2 / p.scale
		case '+', '=':
			p.zoom = min(16, p.zoom*1.25)
		case '-':
			p.zoom = max(0.25, p.zoom/1.25)
		case '0':
			p.zoom, p.panX, p.panY = 1, 0, 0
		}
	}
	p.panX = max(-float64(p.snapshot.Width), min(float64(p.snapshot.Width), p.panX))
	p.panY = max(-float64(p.snapshot.Height), min(float64(p.snapshot.Height), p.panY))
	return false, false
}
func (p *imagePreview) mouse(ev *tcell.EventMouse) (close, confirm bool) {
	x, y := ev.Position()
	if p.details != nil {
		if ev.Buttons()&tcell.WheelUp != 0 {
			p.details.Scroll = max(0, p.details.Scroll-3)
		}
		if ev.Buttons()&tcell.WheelDown != 0 {
			p.details.Scroll += 3
		}
		return false, false
	}
	switch {
	case ev.Buttons()&tcell.WheelUp != 0:
		p.zoom = min(16, p.zoom*1.25)
	case ev.Buttons()&tcell.WheelDown != 0:
		p.zoom = max(.25, p.zoom/1.25)
	case ev.Buttons()&tcell.Button1 != 0:
		if y == p.top+p.height+1 {
			if x >= p.left && x < p.left+10 {
				if p.pending && p.point != nil {
					return true, true
				}
				if !p.pending {
					return true, false
				}
			}
			if x >= p.left+12 && x < p.left+24 {
				return true, false
			}
		}
		if point := p.coordinate(x, y); point != nil {
			p.point = point
		}
	}
	return false, false
}
func (p *imagePreview) draw(s tcell.Screen, g *graphics.Kitty) error {
	if p.details != nil {
		drawWindow(s, p.details)
		return nil
	}
	w, h := s.Size()
	p.geometry(w, h)
	drawWindowFrame(s, &p.Window)
	key := fmt.Sprintf("preview:%s:%dx%d:%dx%dpx:%g:%g:%g", p.snapshot.ID, p.width, p.height, p.cellWidth, p.cellHeight, p.zoom, p.panX, p.panY)
	if p.canvasKey != key {
		// Terminal pixel measurements are external input. Check the canvas
		// before multiplication/allocation, using the graphics upload budget.
		const maxPixels = 16 << 20
		if p.cellWidth > maxPixels/p.width || p.cellHeight > maxPixels/p.height {
			return fmt.Errorf("image preview exceeds 16-megapixel canvas limit")
		}
		pw, ph := p.width*p.cellWidth, p.height*p.cellHeight
		if pw > maxPixels/ph {
			return fmt.Errorf("image preview exceeds 16-megapixel canvas limit")
		}
		canvas := image.NewNRGBA(image.Rect(0, 0, pw, ph))
		bounds := p.pixels.Bounds()
		for y := range ph {
			sy := int(math.Floor((float64(y) - p.originY) / p.scale))
			if sy < 0 || sy >= p.snapshot.Height {
				continue
			}
			for x := range pw {
				sx := int(math.Floor((float64(x) - p.originX) / p.scale))
				if sx >= 0 && sx < p.snapshot.Width {
					canvas.Set(x, y, p.pixels.At(bounds.Min.X+sx, bounds.Min.Y+sy))
				}
			}
		}

		p.canvas = canvas
		p.canvasKey = key
	}
	grid, err := g.Place(key, p.canvas, p.width, p.height, 1)
	if err != nil {
		return err
	}
	style := tcell.StyleDefault.Background(tcell.GetColor(render.SurfaceColor)).Foreground(tcell.GetColor(render.TextColor))
	for y, row := range grid {
		putStyled(s, p.left, p.top+y, p.width, row, style)
	}
	label := fmt.Sprintf("Zoom %.0f%% · source %dx%d", p.zoom*100, p.snapshot.Width, p.snapshot.Height)
	if p.point != nil {
		label += fmt.Sprintf(" · pixel (%d, %d), cell estimate", p.point[0], p.point[1])
	}
	put(s, p.left, p.top+p.height, p.width, label, style.Foreground(tcell.GetColor(render.CyanColor)))
	button := "[ Close ]"
	if p.pending {
		button = "[ OK ]"
		if p.point == nil {
			button = "[ OK: pick ]"
		}
	}
	put(s, p.left, p.top+p.height+1, p.width, fmt.Sprintf("%-12s[ Cancel ]", button), style.Bold(true))
	return nil
}
