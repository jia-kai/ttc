package tui

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ttc/internal/assets"
	"ttc/internal/graphics"
	"ttc/internal/provider"
	"ttc/internal/session"

	"github.com/gdamore/tcell/v2"
)

func isolatedImageFrontend(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	// These image tests do not need optional MathJax host binaries or installs.
	t.Setenv("PATH", t.TempDir())
}

func TestImageRenderingContinuesDuringMathInstallAndExitCancelsIt(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "install-started")
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("TTC_TEST_INSTALL_MARKER", marker)
	for name, script := range map[string]string{
		"node": "#!/bin/sh\nexit 0\n", "rsvg-convert": "#!/bin/sh\nexit 0\n",
		"npm": "#!/bin/sh\n: > \"$TTC_TEST_INSTALL_MARKER\"\nexec /bin/sleep 30\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var output bytes.Buffer
	r, err := newImageRenderer(context.Background(), graphics.New(&output, false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.close() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("math installation did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewNRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "image.png")
	if err := os.WriteFile(path, pngData.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.loadSource(session.ImageSnapshot{ID: "fixture", Snapshot: path}); err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-r.results:
		if !reply.source || reply.err != nil || reply.pixels == nil {
			t.Fatalf("image rendering failed while math initialized: %+v", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("math installation blocked image rendering")
	}
	key := assets.Key("shared-render-root")
	r.tasks <- renderTask{key: key, path: path}
	select {
	case reply := <-r.results:
		if reply.key != key || reply.err != nil || reply.pixels == nil {
			t.Fatalf("thumbnail render failed: %+v", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("math installation blocked thumbnail rendering")
	}
	cacheRoot := filepath.Join(root, "cache", "ttc", "assets")
	ref, err := os.ReadFile(filepath.Join(cacheRoot, "render-"+key+".ref"))
	if err != nil || len(ref) != 64 {
		t.Fatal("renderer did not save its content-addressed recipe reference", err)
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, "blob-"+string(ref)+".blob")); err != nil {
		t.Fatal("renderer did not use shared XDG cache", err)
	}
	start := time.Now()
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("exit did not cancel and join math installation")
	}
}

func TestPreviewCoordinatesPanZoomAndExplicitConfirm(t *testing.T) {
	v := session.ImageSnapshot{ID: "image", Width: 320, Height: 200}
	p := newImagePreview(v, image.NewNRGBA(image.Rect(0, 0, 320, 200)), true, 10, 20)
	p.geometry(100, 30)
	if p.coordinate(p.left, p.top+10) != nil {
		t.Fatal("letterbox generated coordinate")
	}
	x, y := 50, 14
	point := p.coordinate(x, y)
	if point == nil || point[0] < 155 || point[0] > 165 || point[1] < 95 || point[1] > 110 {
		t.Fatal(point)
	}
	close, confirm := p.mouse(tcell.NewEventMouse(x, y, tcell.Button1, 0))
	if close || confirm || p.point == nil {
		t.Fatal("click submitted without OK")
	}
	p.key(tcell.NewEventKey(tcell.KeyRune, 'l', 0))
	p.geometry(100, 30)
	pan := p.coordinate(x, y)
	if pan == nil || pan[0] <= point[0] {
		t.Fatal("pan not mapped", pan)
	}
	p.key(tcell.NewEventKey(tcell.KeyRune, '+', 0))
	p.geometry(100, 30)
	if p.zoom <= 1 {
		t.Fatal("zoom failed")
	}
	close, confirm = p.key(tcell.NewEventKey(tcell.KeyEnter, 0, 0))
	if !close || !confirm {
		t.Fatal("OK did not confirm")
	}
	p.key(tcell.NewEventKey(tcell.KeyRune, '0', 0))
	p.geometry(70, 20)
	if p.zoom != 1 || p.panX != 0 || p.panY != 0 {
		t.Fatal("fit failed")
	}
	p.pending = false
	close, confirm = p.key(tcell.NewEventKey(tcell.KeyEscape, 0, 0))
	if !close || confirm {
		t.Fatal("ordinary dismiss confirmed a click")
	}
}

func TestPendingImageRemainsReachableAfterUICompaction(t *testing.T) {
	isolatedImageFrontend(t)
	p := &provider.Script{Responses: []provider.ScriptResponse{
		{Calls: []provider.ToolCall{{ID: "show", Name: "image_show", Arguments: []byte(`{"path":"field.png","request_click":true}`)}}},
		{Text: "Select the image."}, {Text: "The image selection is still pending."}, {Prefix: "user: {", Text: "Archived image selection received."},
	}}
	var output bytes.Buffer
	u := newQuestionTestUI(t, p, graphics.New(&output, false))
	f, err := os.Create(filepath.Join(u.runtime.Workspace.Root, "field.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err = png.Encode(f, image.NewNRGBA(image.Rect(0, 0, 320, 200))); err != nil {
		t.Fatal(err)
	}
	f.Close()
	u.typeText("show the image")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	u.wait(t, "click pending")
	var entry int64
	if err = u.runtime.Store.DB.QueryRow("SELECT r.entry_id FROM tool_records r JOIN tool_calls c ON c.id=r.call_id WHERE c.name='image_show'").Scan(&entry); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{strings.Repeat("old research result ", 3000), "Continue the pending image interaction."} {
		if _, err = u.runtime.Store.Append(u.runtime.Current(), "", "main", "message", "user", true, provider.Message{Role: "user", Content: text}); err != nil {
			t.Fatal(err)
		}
	}
	u.typeText("/compact")
	u.key(tcell.KeyEnter)
	u.wait(t, "Command result")
	u.key(tcell.KeyEscape)
	u.wait(t, "click pending")
	branch, err := u.runtime.Store.Branch(u.runtime.Current(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range branch {
		if v.ID == entry {
			t.Fatal("fixture did not archive the original image entry")
		}
	}
	u.typeText(fmt.Sprintf("/inspect %d", entry))
	u.key(tcell.KeyEnter)
	u.wait(t, "h/j/k/l pan")
	u.screen.PostEventWait(tcell.NewEventMouse(50, 14, tcell.Button1, 0))
	u.wait(t, "pixel (")
	u.key(tcell.KeyEnter)
	u.wait(t, "Archived image selection received.")
}

func TestConfirmOlderImageFollowsResumedReply(t *testing.T) {
	isolatedImageFrontend(t)
	p := &provider.Script{Responses: []provider.ScriptResponse{
		{Calls: []provider.ToolCall{{ID: "show", Name: "image_show", Arguments: []byte("{\"path\":\"field.png\",\"request_click\":true}")}}},
		{Text: strings.Repeat("More research results.\n", 60)},
		{Prefix: "user: {", Text: "Confirmed point response visible."},
	}}
	var output bytes.Buffer
	u := newQuestionTestUI(t, p, graphics.New(&output, false))
	f, err := os.Create(filepath.Join(u.runtime.Workspace.Root, "field.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, image.NewNRGBA(image.Rect(0, 0, 320, 200))); err != nil {
		t.Fatal(err)
	}
	f.Close()
	u.typeText("show image")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	// Scroll the actual conversation, then click the archived thumbnail. /inspect
	// would reset followTail during submission and conceal the original bug.
	for range 20 {
		u.key(tcell.KeyCtrlU)
	}
	// A composer marker proves all preceding scroll events were processed.
	// Otherwise an older queued frame can supply coordinates while later
	// scroll events move the card before the mouse click is delivered.
	u.typeText("viewport ready")
	deadline := time.After(3 * time.Second)
	y := -1
	for y < 0 {
		select {
		case frame := <-u.screen.frames:
			if !strings.Contains(frame, "viewport ready") {
				continue
			}
			for row, line := range strings.Split(frame, "\n") {
				if strings.Contains(line, "image_show") && !strings.Contains(line, "awaiting") {
					y = row
					break
				}
			}
		case <-deadline:
			t.Fatal("image card not found after scrolling")
		}
	}
	u.screen.PostEventWait(tcell.NewEventMouse(4, y, tcell.Button1, 0))
	u.wait(t, "h/j/k/l pan")
	u.screen.PostEventWait(tcell.NewEventMouse(50, 14, tcell.Button1, 0))
	u.wait(t, "pixel (")
	u.key(tcell.KeyEnter)
	u.wait(t, "Confirmed point response visible.")
}

func TestFormulaPressureSchedulesOnlyViewportAndSettles(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output bytes.Buffer
	g := graphics.New(&output, false)
	r := &imageRenderer{graphics: g, cellWidth: 8, cellHeight: 16, backend: "test", ctx: ctx, tasks: make(chan renderTask, 32), pending: map[string]context.CancelFunc{}, ready: map[string]renderReply{}, visible: map[string]bool{}}
	v := newTranscript()
	v.layout = r.layout
	var source strings.Builder
	for i := range 600 {
		fmt.Fprintf(&source, "formula $x_{%d}$\n\n", i)
	}
	v.append(line{text: source.String(), markdown: true})
	queued := 0
	frame := func() int {
		g.Begin()
		rows := v.viewport(80, 25)
		if err := r.ensure(rows); err != nil {
			t.Fatal(err)
		}
		if err := g.End(); err != nil {
			t.Fatal(err)
		}
		n := 0
		for len(r.tasks) > 0 {
			task := <-r.tasks
			n++
			queued++
			if r.accept(renderReply{key: task.key, pixels: image.NewNRGBA(image.Rect(0, 0, 32*assets.MathRasterScale, 12*assets.MathRasterScale)), math: true}) {
				v.invalidate()
			}
		}
		return n
	}
	for i := range 20 {
		if frame() == 0 {
			break
		}
		if i == 19 {
			t.Fatal("viewport never settled")
		}
	}
	if queued >= 128 || len(r.ready) > 25 {
		t.Fatal("scheduled nonvisible formulas", queued, len(r.ready))
	}
	before := queued
	bytes := output.Len()
	for range 100 {
		if frame() != 0 {
			t.Fatal("idle frame rescheduled a formula")
		}
	}
	if queued != before || output.Len() != bytes {
		t.Fatal("idle graphics thrash")
	}
	v.scroll(-200, 25)
	for i := range 20 {
		if frame() == 0 {
			break
		}
		if i == 19 {
			t.Fatal("scrolled viewport never settled")
		}
	}
	if len(r.ready) > 25 || r.imageBytes > 32<<20 {
		t.Fatal("unbounded decoded working set")
	}
}
func TestFormulaLayoutUsesImagesPromotesAndKeepsCode(t *testing.T) {
	var output bytes.Buffer
	g := graphics.New(&output, false)
	g.Begin()
	r := &imageRenderer{graphics: g, cellWidth: 8, cellHeight: 16, backend: "test", ready: map[string]renderReply{}}
	for _, tex := range []string{"x^2", `\frac{1}{n}`} {
		key := assets.Key("mathjax-hires-v1", r.backend, tex, "16", fmt.Sprint(assets.MathRasterScale))
		height := 12
		if strings.Contains(tex, "frac") {
			height = 40
		}
		r.ready[key] = renderReply{pixels: image.NewNRGBA(image.Rect(0, 0, 40*assets.MathRasterScale, height*assets.MathRasterScale))}
	}
	rows := r.layout(line{text: "before $x^2$ after\n\ninline $\\frac{1}{n}$ end\n\n`$x^2$`", markdown: true}, 80)
	var texts []string
	for _, row := range rows {
		texts = append(texts, row.text)
	}
	text := strings.Join(texts, "\n")
	if !strings.ContainsRune(text, '\U0010eeee') {
		t.Fatal("math did not use placeholders", text)
	}
	if !strings.Contains(text, "$x^2$") {
		t.Fatal("code example changed", text)
	}
	imageRows := 0
	for _, row := range rows {
		if strings.ContainsRune(row.text, '\U0010eeee') {
			imageRows++
			if len(row.assets) != 1 {
				t.Fatal("row inherited unrelated assets", row)
			}
		}
	}
	if imageRows < 4 {
		t.Fatal("Markdown folded image rows together", text)
	}
	r.backendError = context.DeadlineExceeded
	rows = r.layout(line{text: `$\alpha$`, markdown: true}, 80)
	if len(rows) == 0 || !strings.Contains(rows[0].text, `\alpha`) {
		t.Fatal("missing backend lost TeX")
	}
}

func TestHighResolutionFormulaPlacementAndCache(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		cellWidth, cellHeight int
		width, height, pane   int
		columns, rows         int
	}{
		{"inline", 8, 16, 40, 12, 80, 5, 1},
		{"fraction", 8, 16, 40, 40, 80, 5, 3},
		{"larger terminal font", 12, 24, 80, 20, 80, 7, 1},
		{"narrow pane", 8, 16, 80, 12, 16, 10, 1},
		{"tall formula", 8, 16, 400, 1000, 30, 10, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := graphics.New(&bytes.Buffer{}, false)
			r := &imageRenderer{graphics: g, backend: "test", cellWidth: tc.cellWidth, cellHeight: tc.cellHeight, ready: map[string]renderReply{}}
			key := assets.Key("mathjax-hires-v1", r.backend, "x", fmt.Sprint(tc.cellHeight), fmt.Sprint(assets.MathRasterScale))
			r.ready[key] = renderReply{pixels: image.NewNRGBA(image.Rect(0, 0, tc.width*assets.MathRasterScale, tc.height*assets.MathRasterScale))}
			rows := r.layout(line{text: "$x$", markdown: true}, tc.pane)
			found := false
			for _, row := range rows {
				for _, asset := range row.assets {
					found = true
					if asset.columns != tc.columns || asset.rows != tc.rows {
						t.Fatalf("supersampling changed placement: %dx%d, want %dx%d", asset.columns, asset.rows, tc.columns, tc.rows)
					}
				}
			}
			if !found {
				t.Fatal("formula lost its placement")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := assets.Key("mathjax-v1", "test", "x", "16")
	r := &imageRenderer{graphics: graphics.New(&bytes.Buffer{}, false), backend: "test", cellWidth: 8, cellHeight: 16, ctx: ctx, tasks: make(chan renderTask, 1), pending: map[string]context.CancelFunc{}, ready: map[string]renderReply{old: {pixels: image.NewNRGBA(image.Rect(0, 0, 40, 12))}}}
	rows := r.layout(line{text: "$x$", markdown: true}, 80)
	var visible []line
	for _, row := range rows {
		if strings.ContainsRune(row.text, '\U0010eeee') {
			t.Fatal("reused a low-resolution cached formula")
		}
		visible = append(visible, line{text: row.text, assets: row.assets})
	}
	if err := r.ensure(visible); err != nil {
		t.Fatal(err)
	}
	select {
	case task := <-r.tasks:
		if task.key == old || task.pixels != 14 {
			t.Fatal("cache refresh changed logical font size", task)
		}
	default:
		t.Fatal("high-resolution formula was not queued")
	}
}

func TestCellMeasurementFallbackWarningAndRecovery(t *testing.T) {
	r := &imageRenderer{graphics: graphics.New(&bytes.Buffer{}, false), cellWidth: 8, cellHeight: 16}
	for _, tc := range []struct {
		name    string
		size    tcell.WindowSize
		cw, ch  int
		changed bool
		warn    bool
	}{
		{"missing", tcell.WindowSize{}, 8, 16, false, true},
		{"still missing", tcell.WindowSize{Width: 80, Height: 24}, 8, 16, false, false},
		{"measured", tcell.WindowSize{Width: 80, Height: 24, PixelWidth: 960, PixelHeight: 576}, 12, 24, true, false},
		{"same", tcell.WindowSize{Width: 80, Height: 24, PixelWidth: 960, PixelHeight: 576}, 12, 24, false, false},
		{"partial", tcell.WindowSize{Width: 80, Height: 24, PixelWidth: 960}, 8, 16, true, true},
		{"zero grid", tcell.WindowSize{PixelWidth: 960, PixelHeight: 576}, 8, 16, false, false},
		{"negative", tcell.WindowSize{Width: -1, Height: 24, PixelWidth: 960, PixelHeight: 576}, 8, 16, false, false},
		{"subpixel cell", tcell.WindowSize{Width: 80, Height: 24, PixelWidth: 1, PixelHeight: 1}, 8, 16, false, false},
		{"fallback sized measurement", tcell.WindowSize{Width: 80, Height: 24, PixelWidth: 640, PixelHeight: 384}, 8, 16, false, false},
		{"missing again", tcell.WindowSize{}, 8, 16, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed, warning := r.updateCellDimensions(tc.size)
			if changed != tc.changed || (warning != "") != tc.warn || r.cellWidth != tc.cw || r.cellHeight != tc.ch {
				t.Fatal("incorrect fallback transition", changed, warning, r.cellWidth, r.cellHeight)
			}
			if r.graphics.CellWidth != tc.cw || r.graphics.CellHeight != tc.ch {
				t.Fatal("upload geometry not synchronized")
			}
		})
	}
}

func TestFallbackWarningIsVisibleAndNotModelContext(t *testing.T) {
	isolatedImageFrontend(t)
	u := newQuestionTestUI(t, &provider.Script{Responses: []provider.ScriptResponse{{Text: "Warning check complete."}}}, graphics.New(&bytes.Buffer{}, false))
	u.typeText("draw marker")
	frame := u.wait(t, "draw marker")
	if strings.Count(frame, "Warning: using estimated") != 1 || !strings.Contains(frame, "8×16") {
		t.Fatal("missing or repeated fallback warning", frame)
	}
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	messages, err := u.runtime.Store.Messages(u.runtime.Current())
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if strings.Contains(message.Content, "Warning: using estimated") {
			t.Fatal("presentation warning entered model context")
		}
	}
}

func TestPreviewRebuildsAtNewCellPixelSize(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(50, 20)
	g := graphics.New(&bytes.Buffer{}, false)
	p := newImagePreview(session.ImageSnapshot{ID: "image", Width: 20, Height: 10}, image.NewRGBA(image.Rect(0, 0, 20, 10)), false, 8, 16)
	g.Begin()
	if err := p.draw(s, g); err != nil {
		t.Fatal(err)
	}
	old := p.canvasKey
	p.cellWidth, p.cellHeight = 10, 20
	g.CellWidth, g.CellHeight = 10, 20
	if err := p.draw(s, g); err != nil {
		t.Fatal(err)
	}
	if p.canvasKey == old || p.canvas.Bounds().Dx() != p.width*10 || p.canvas.Bounds().Dy() != p.height*20 {
		t.Fatal("preview canvas retained stale pixel geometry")
	}
}
func TestImageFrontendPreviewAndClickNotification(t *testing.T) {
	isolatedImageFrontend(t)
	p := &provider.Script{Responses: []provider.ScriptResponse{{Calls: []provider.ToolCall{{ID: "show", Name: "image_show", Arguments: []byte(`{"path":"field.png","request_click":true}`)}}}, {Text: "Click the image."}, {Prefix: "user: {", Text: "Coordinate received."}}}
	var output bytes.Buffer
	g := graphics.New(&output, false)
	u := newQuestionTestUI(t, p, g)
	path := filepath.Join(u.runtime.Workspace.Root, "field.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = png.Encode(f, image.NewNRGBA(image.Rect(0, 0, 320, 200))); err != nil {
		t.Fatal(err)
	}
	f.Close()
	u.typeText("show image")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	u.wait(t, "click pending")
	var id int64
	if err = u.runtime.Store.DB.QueryRow("SELECT r.entry_id FROM tool_records r JOIN tool_calls c ON c.id=r.call_id WHERE c.name='image_show'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	u.typeText(fmt.Sprintf("/inspect %d", id))
	u.key(tcell.KeyEnter)
	u.wait(t, "h/j/k/l pan")
	u.typeText("i")
	u.wait(t, "Parameters:")
	u.key(tcell.KeyEscape)
	u.wait(t, "h/j/k/l pan")
	u.screen.PostEventWait(tcell.NewEventMouse(50, 14, tcell.Button1, 0))
	u.wait(t, "pixel (")
	var count int
	if err := u.runtime.Store.DB.QueryRow("SELECT count(*) FROM model_requests WHERE purpose='coding'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatal("click submitted before OK", count)
	}
	u.key(tcell.KeyEnter)
	u.wait(t, "Coordinate received.")
	last, err := u.runtime.Store.Messages(u.runtime.Current())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range last {
		if strings.Contains(m.Content, `"type":"image_click"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("model missed confirmed coordinate")
	}
	// History inspection opens the saved pixels without reviving an interaction.
	u.typeText(fmt.Sprintf("/inspect %d", id))
	u.key(tcell.KeyEnter)
	u.wait(t, "[ Close ]")
	u.key(tcell.KeyEscape)
	time.Sleep(20 * time.Millisecond)
	if u.runtime.HasNotifications() {
		t.Fatal("ordinary preview emitted cancellation")
	}
}

func TestPastePreservesImagePreviewWithDismissedQuestion(t *testing.T) {
	isolatedImageFrontend(t)
	p := &provider.Script{Responses: []provider.ScriptResponse{
		{Calls: []provider.ToolCall{{ID: "show", Name: "image_show", Arguments: []byte(`{"path":"field.png"}`)}}},
		questionScript()[0],
		{Text: "Answered."},
	}}
	var output bytes.Buffer
	u := newQuestionTestUI(t, p, graphics.New(&output, false))
	f, err := os.Create(filepath.Join(u.runtime.Workspace.Root, "field.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err = png.Encode(f, image.NewNRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	u.typeText("show and ask")
	u.key(tcell.KeyEnter)
	u.wait(t, "Choose a method?")
	u.key(tcell.KeyEscape)
	var entry int64
	if err = u.runtime.Store.DB.QueryRow("SELECT r.entry_id FROM tool_records r JOIN tool_calls c ON c.id=r.call_id WHERE c.name='image_show'").Scan(&entry); err != nil {
		t.Fatal(err)
	}
	u.typeText(fmt.Sprintf("/inspect %d", entry))
	u.key(tcell.KeyEnter)
	u.wait(t, "h/j/k/l pan")
	u.screen.PostEventWait(tcell.NewEventPaste(true))
	u.typeText("ignore pasted keys")
	u.key(tcell.KeyEnter)
	u.screen.PostEventWait(tcell.NewEventPaste(false))
	u.typeText("+")
	u.wait(t, "Zoom 125%")
}
