package tui

import (
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/assets"
	"ttc/internal/graphics"
	"ttc/internal/llm"
	"ttc/internal/session"
)

func markdownImageFixture(t *testing.T, directory, name string, width, height int) string {
	t.Helper()
	path := filepath.Join(directory, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLocalMarkdownImagePaths(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		source, path string
		local        bool
	}{
		{"space%20image.png", filepath.Join(root, "space image.png"), true},
		{"literal%2520.png", filepath.Join(root, "literal%20.png"), true},
		{"file:///tmp/local%20image.png", "/tmp/local image.png", true},
		{"file://localhost/tmp/image.png", "/tmp/image.png", true},
		{"../other.png", filepath.Join(filepath.Dir(root), "other.png"), true},
		{"https://example.test/a.png", "", false},
		{"//example.test/a.png", "", false},
		{"file://remote/tmp/image.png", "", false},
		{"data:image/png;base64,AAAA", "", false},
	} {
		path, local := localImagePath(root, test.source)
		if path != test.path || local != test.local {
			t.Errorf("%q: %q,%v; want %q,%v", test.source, path, local, test.path, test.local)
		}
	}
}

func TestMarkdownImagesUseAtomicBoxesAndExactHitSpans(t *testing.T) {
	isolatedImageFrontend(t)
	root := t.TempDir()
	path := markdownImageFixture(t, root, "figure.png", 120, 48)
	r, err := newImageRenderer(context.Background(), graphics.New(io.Discard, false), root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	key := assets.Key("local-thumbnail-v1", path)
	r.ready[key] = renderReply{pixels: image.NewRGBA(image.Rect(0, 0, 120, 48))}
	r.imageBytes = 120 * 48 * 4
	for _, source := range []string{
		"before $x$ ![plot](figure.png) after",
		"- nested ![plot](figure.png) caption",
		"| figure | caption |\n| --- | --- |\n| ![plot](figure.png) | data |",
	} {
		rows := r.layout(line{text: source, markdown: true}, 50)
		found := false
		for _, row := range rows {
			for _, asset := range row.assets {
				if !asset.local {
					continue
				}
				found = true
				if !asset.clickable || asset.path != path || asset.columns > 50 {
					t.Fatalf("invalid local image: %+v", asset)
				}
				for x := asset.column; x < asset.column+asset.columns; x++ {
					if imageAt(row.assets, x) == nil {
						t.Fatalf("image cell %d missed: %+v", x, asset)
					}
				}
				if imageAt(row.assets, asset.column-1) != nil || imageAt(row.assets, asset.column+asset.columns) != nil {
					t.Fatalf("neighbor text/formula targeted image: %+v", row.assets)
				}
			}
		}
		if !found {
			t.Fatalf("image missing from %q", source)
		}
	}
	// Transcript attribution prefixes must move hit spans with rendered cells.
	view := newTranscript()
	view.layout = r.layout
	view.append(line{text: "before ![plot](figure.png)", markdown: true, speaker: "Assistant"})
	rows := view.viewport(50, 20)
	for _, row := range rows {
		for _, asset := range row.assets {
			if asset.local && asset.column < 2 {
				t.Fatal("speaker indentation not reflected in image hit span")
			}
		}
	}
}

func TestMarkdownImagesLoadOnlyVisibleAssetsAndReportFailures(t *testing.T) {
	isolatedImageFrontend(t)
	root := t.TempDir()
	path := markdownImageFixture(t, root, "visible.png", 16, 16)
	r, err := newImageRenderer(context.Background(), graphics.New(io.Discard, false), root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	source := "![visible](visible.png)\n\n" + strings.Repeat("padding\n\n", 50) + "![offscreen](missing.png)"
	window := &Window{Text: source, Markdown: true, renderer: r}
	rows := window.Lines(80, 4)
	visible := make([]line, len(rows))
	for i := range rows {
		row := window.assets[window.Scroll+i]
		visible[i] = line{text: row.text, assets: row.assets}
	}
	if len(r.pending) != 0 {
		t.Fatal("layout performed IO")
	}
	if err := r.ensure(visible); err != nil {
		t.Fatal(err)
	}
	expected := assets.Key("local-thumbnail-v1", path)
	if len(r.pending) != 1 || r.pending[expected] == nil {
		t.Fatalf("nonvisible image queued: %v", r.pending)
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	loaded := false
	for !loaded {
		select {
		case reply := <-r.results:
			r.accept(reply)
			loaded = reply.key == expected && reply.pixels != nil
		case <-deadline.C:
			t.Fatal("visible image did not load")
		}
	}
	if rows := window.Lines(80, 4); !strings.ContainsRune(strings.Join(rows, "\n"), '\U0010eeee') {
		t.Fatal("loaded image did not replace placeholder")
	}
	missing := filepath.Join(root, "missing.png")
	r.ready[assets.Key("local-thumbnail-v1", missing)] = renderReply{err: os.ErrNotExist}
	for _, source := range []string{"![missing alt](missing.png)", "![remote alt](https://example.test/image.png)", "![data alt](data:image/png;base64,AAAA)"} {
		rows := r.layout(line{text: source, markdown: true}, 80)
		var text strings.Builder
		for _, row := range rows {
			text.WriteString(row.text)
		}
		if !strings.Contains(text.String(), "alt") {
			t.Fatalf("image fallback lost alt text: %s", text.String())
		}
		if strings.Contains(source, "missing") && !strings.Contains(text.String(), "unavailable") {
			t.Fatal("missing local image not labeled")
		}
		if !strings.Contains(source, "missing") {
			for _, row := range rows {
				if len(row.assets) != 0 {
					t.Fatal("remote image scheduled for loading")
				}
			}
		}
	}
}

func TestDocumentLocalImageClickPreviewAndReturn(t *testing.T) {
	isolatedImageFrontend(t)
	root := t.TempDir()
	markdownImageFixture(t, root, "space image.png", 64, 32)
	screen := &observedScreen{Screen: newTestScreen(t), frames: make(chan string, 64)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- (&Document{Title: "local.md", Directory: root, Text: "![plot](space%20image.png)", Screen: screen, Graphics: graphics.New(io.Discard, false)}).Run(ctx)
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("viewer did not exit")
		}
	}()
	frame := waitDocumentFrame(t, screen.frames, string('\U0010eeee'))
	x, y := -1, -1
	for row, line := range strings.Split(frame, "\n") {
		if strings.ContainsRune(line, '\U0010eeee') {
			x = 0
			y = row
			break
		}
	}
	if y < 0 {
		t.Fatal("image not visible")
	}
	screen.EventQ() <- tcell.NewEventMouse(x, y, tcell.Button1, 0)
	waitDocumentFrame(t, screen.frames, "Image · space image.png")
	screen.EventQ() <- tcell.NewEventKey(tcell.KeyRune, "+", 0)
	waitDocumentFrame(t, screen.frames, "Zoom 125%")
	screen.EventQ() <- tcell.NewEventKey(tcell.KeyEscape, "", 0)
	waitDocumentFrame(t, screen.frames, "local.md")
}

func TestMarkdownImageChatPreviewReturns(t *testing.T) {
	isolatedImageFrontend(t)
	provider := &llm.Script{Responses: []llm.ScriptResponse{{Text: "before ![chart](chart.png) after"}}}
	u := newQuestionTestUI(t, provider, graphics.New(io.Discard, false))
	markdownImageFixture(t, u.runtime.Workspace.Root, "chart.png", 32, 16)
	u.typeText("show chart")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	frame := u.wait(t, string('\U0010eeee'))
	// Click the exact source cells rather than the assistant's whole row.
	x, y := -1, -1
	for row, text := range strings.Split(frame, "\n") {
		column := 0
		for _, r := range text {
			if r == '\U0010eeee' {
				x, y = column, row
				break
			}
			column++
		}
		if x >= 0 {
			break
		}
	}
	if x < 0 {
		t.Fatal("chat thumbnail missing")
	}
	u.screen.EventQ() <- tcell.NewEventMouse(x, y, tcell.Button1, 0)
	u.wait(t, "Image · chart.png")
	u.key(tcell.KeyEscape)
	u.wait(t, "before")
}

func TestMarkdownImageInspectorReturnsToPriorWindow(t *testing.T) {
	isolatedImageFrontend(t)
	root := t.TempDir()
	path := markdownImageFixture(t, root, "chart.png", 32, 16)
	r, err := newImageRenderer(context.Background(), graphics.New(io.Discard, false), root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	key := assets.Key("local-thumbnail-v1", path)
	r.ready[key] = renderReply{pixels: image.NewRGBA(image.Rect(0, 0, 32, 16))}
	r.imageBytes = 32 * 16 * 4
	screen := newTestScreen(t)
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	window := &Window{Title: "Markdown inspector", Text: "before ![chart](chart.png) after", Markdown: true, renderer: r}
	drawWindow(screen, window)
	var asset placedImage
	row := -1
	for i, rendered := range window.assets {
		for _, p := range rendered.assets {
			if p.clickable {
				asset = p
				row = i
				break
			}
		}
	}
	if row < 0 {
		t.Fatal("inspector thumbnail missing")
	}
	c := &frontendController{screen: screen, renderer: r, frontend: &Frontend{Runtime: &session.Runtime{}}, view: newTranscript(), modal: modalState{window: window}, names: map[string]string{}}
	w, h := screen.Size()
	left, top, _, _ := windowBounds(w, h)
	markdownImageFixture(t, root, "chart.png", 64, 24)
	c.handleMouse(tcell.NewEventMouse(left+1+asset.column, top+1+row, tcell.Button1, 0))
	if c.modal.loading == nil {
		t.Fatal("inspector image click did not load preview")
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for c.modal.preview == nil {
		select {
		case reply := <-r.results:
			c.handleRenderResult(reply)
		case <-deadline.C:
			t.Fatal("inspector preview did not load")
		}
	}
	if c.modal.preview.pending {
		t.Fatal("local image requested agent coordinates")
	}
	if c.modal.preview.sourceWidth != 64 || c.modal.preview.sourceHeight != 24 {
		t.Fatal("preview used stale thumbnail dimensions")
	}
	c.finishPreview(false)
	if c.modal.window != window {
		t.Fatal("preview did not restore original inspector")
	}
	c.openLocalImage(asset)
	load := c.modal.loading
	c.finishPreview(false)
	c.handleRenderResult(renderReply{source: true, key: load.key, pixels: image.NewRGBA(image.Rect(0, 0, 8, 8))})
	if c.modal.window != window || c.modal.preview != nil {
		t.Fatal("late source result revived dismissed preview")
	}
}

func TestMarkdownImageThumbnailCacheTracksSourceBytes(t *testing.T) {
	isolatedImageFrontend(t)
	root := t.TempDir()
	for _, width := range []int{16, 64} {
		path := markdownImageFixture(t, root, "mutable.png", width, 16)
		r, err := newImageRenderer(context.Background(), graphics.New(io.Discard, false), root)
		if err != nil {
			t.Fatal(err)
		}
		rows := r.layout(line{text: "![mutable](mutable.png)", markdown: true}, 80)
		visible := make([]line, len(rows))
		for i, row := range rows {
			visible[i] = line{text: row.text, assets: row.assets}
		}
		if err := r.ensure(visible); err != nil {
			r.close()
			t.Fatal(err)
		}
		key := assets.Key("local-thumbnail-v1", path)
		deadline := time.NewTimer(3 * time.Second)
		loaded := false
		for !loaded {
			select {
			case reply := <-r.results:
				r.accept(reply)
				loaded = reply.key == key
			case <-deadline.C:
				r.close()
				t.Fatal("mutable thumbnail did not load")
			}
		}
		deadline.Stop()
		pixels := r.ready[key].pixels
		if pixels == nil || pixels.Bounds().Dx() != width {
			r.close()
			t.Fatalf("stale thumbnail for width %d: %v", width, pixels)
		}
		if err := r.close(); err != nil {
			t.Fatal(err)
		}
	}
}
