package tui

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"strings"
	"testing"

	"ttc/internal/assets"
	"ttc/internal/graphics"
)

func TestMathRasterLRULimitsAndRecency(t *testing.T) {
	c := &mathRasterCache{}
	p := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for _, key := range []string{"a", "b"} {
		if ok, _ := c.put(key, p, 32); !ok {
			t.Fatal("small raster rejected")
		}
	}
	c.get("a")
	if ok, removed := c.put("c", p, 32); !ok || len(removed) != 1 || removed[0] != "b" {
		t.Fatalf("incorrect LRU eviction: %v %v", ok, removed)
	}
	if c.get("a") == nil || c.get("b") != nil || c.bytes != 32 {
		t.Fatal("wrong cache contents or byte count")
	}
	c.put("a", image.NewRGBA(image.Rect(0, 0, 1, 1)), 32)
	if c.bytes != 20 || c.recent.Len() != 2 {
		t.Fatal("replacement double-counted memory")
	}
	if ok, _ := c.put("oversized", p, 8); ok {
		t.Fatal("oversized raster accepted")
	}
	for i := range mathRasterEntries + 20 {
		c.put(fmt.Sprint(i), p, decodedImageLimit)
	}
	if c.recent.Len() != mathRasterEntries || c.bytes != mathRasterEntries*16 {
		t.Fatal("entry limit exceeded", c.recent.Len(), c.bytes)
	}
	c.trim(0)
	if c.bytes != 0 || len(c.entries) != 0 {
		t.Fatal("zero budget retained rasters")
	}
}

func TestVisibleReadyFormulaRefreshesLRURecency(t *testing.T) {
	r := mathCacheTestRenderer()
	r.mathCache = &mathRasterCache{}
	p := image.NewRGBA(image.Rect(0, 0, 1, 1))
	r.mathCache.put("a", p, 8)
	r.mathCache.put("b", p, 8)
	r.ready["a"] = renderReply{key: "a", pixels: p, math: true}
	r.cachedMath("a")
	if _, removed := r.mathCache.put("c", p, 8); len(removed) != 1 || removed[0] != "b" {
		t.Fatal("visible ready hit did not refresh LRU recency", removed)
	}
}

func mathCacheTestRenderer() *imageRenderer {
	return &imageRenderer{graphics: graphics.New(&bytes.Buffer{}, false), ctx: context.Background(),
		backend: "fixture", cellWidth: 8, cellHeight: 16, ready: map[string]renderReply{},
		pending: map[string]context.CancelFunc{}, tasks: make(chan renderTask, 32)}
}

func mathCacheRows(rows []displayRow) []line {
	var out []line
	for _, row := range rows {
		out = append(out, line{text: row.text, assets: row.assets})
	}
	return out
}

func TestFormulaScrollReturnUsesRasterBeforeFirstPaint(t *testing.T) {
	r := mathCacheTestRenderer()
	defer r.graphics.Close()
	paint := func(tex string) []displayRow {
		t.Helper()
		rows := r.layout(line{text: "before $" + tex + "$ after", markdown: true}, 80)
		r.graphics.Begin()
		if err := r.ensure(mathCacheRows(rows)); err != nil {
			t.Fatal(err)
		}
		if err := r.graphics.End(); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	var a string
	for _, tex := range []string{"x^2", "y^2"} {
		paint(tex)
		if len(r.tasks) != 1 {
			t.Fatal("initial formula did not request one raster")
		}
		task := <-r.tasks
		if a == "" {
			a = task.key
		}
		r.accept(renderReply{key: task.key, pixels: image.NewRGBA(image.Rect(0, 0, 72, 36)), math: true})
		paint(tex)
	}
	if _, ok := r.ready[a]; ok {
		t.Fatal("nonvisible ready raster was not released")
	}
	rows := paint("x^2")
	var text strings.Builder
	for _, row := range rows {
		text.WriteString(row.text)
	}
	if !strings.ContainsRune(text.String(), '\U0010eeee') || strings.Contains(text.String(), "x^2") {
		t.Fatal("first return paint exposed TeX instead of cached pixels")
	}
	if len(r.tasks) != 0 || len(r.pending) != 0 {
		t.Fatal("cache hit scheduled filesystem/MathJax work")
	}
	// A backend or cell-height change must never reuse incompatible pixels.
	for _, change := range []func(){func() { r.cellHeight = 20 }, func() { r.backend = "other" }} {
		change()
		rows = paint("x^2")
		if len(r.tasks) != 1 {
			t.Fatal("incompatible raster key reused")
		}
		task := <-r.tasks
		if task.key == a {
			t.Fatal("render dimensions/backend absent from key")
		}
		r.accept(renderReply{key: task.key, pixels: image.NewRGBA(image.Rect(0, 0, 72, 36)), math: true})
	}
}

func TestFormulaTranscriptScrollReturnDoesNotScheduleRender(t *testing.T) {
	r := mathCacheTestRenderer()
	defer r.graphics.Close()
	v := newTranscript()
	v.layout = r.layout
	v.append(line{text: "first $x^2$", markdown: true})
	for range 60 {
		v.append(line{text: "ordinary history line"})
	}
	v.append(line{text: "last $y^2$", markdown: true})
	frame := func() []line {
		t.Helper()
		rows := v.viewport(80, 8)
		r.graphics.Begin()
		if err := r.ensure(rows); err != nil {
			t.Fatal(err)
		}
		if err := r.graphics.End(); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	settle := func() {
		t.Helper()
		for range 5 {
			frame()
			if len(r.tasks) == 0 {
				return
			}
			for len(r.tasks) > 0 {
				task := <-r.tasks
				if r.accept(renderReply{key: task.key, pixels: image.NewRGBA(image.Rect(0, 0, 72, 36)), math: true}) {
					v.invalidate()
				}
			}
		}
		t.Fatal("formula viewport did not settle")
	}
	v.followTail = false
	settle()
	v.followTail = true
	settle()
	v.followTail, v.firstLine, v.resolveAnchor = false, 0, false
	rows := frame()
	var text strings.Builder
	for _, row := range rows {
		text.WriteString(row.text)
	}
	if !strings.ContainsRune(text.String(), '\U0010eeee') || strings.Contains(text.String(), "x^2") || len(r.tasks) != 0 {
		t.Fatal("scroll return exposed source or scheduled a render", text.String(), len(r.tasks))
	}
}

// countedRaster avoids allocating large test buffers; the accounting uses the
// decoded RGBA bounds, while no upload occurs in this memory-pressure test.
type countedRaster struct{ image.Rectangle }

func (m countedRaster) Bounds() image.Rectangle { return m.Rectangle }
func (m countedRaster) ColorModel() color.Model { return color.RGBAModel }
func (m countedRaster) At(int, int) color.Color { return color.RGBA{} }

func TestMathAndThumbnailsShareDecodedMemoryBudget(t *testing.T) {
	r := mathCacheTestRenderer()
	r.visible = map[string]bool{"math": true, "thumb": true, "source": true, "bad": true}
	m := countedRaster{image.Rect(0, 0, 2048, 2560)} // 20 MiB decoded.
	r.accept(renderReply{key: "math", pixels: m, math: true})
	if r.mathCache.bytes != 20<<20 {
		t.Fatal("formula missing from memory cache")
	}
	r.accept(renderReply{key: "thumb", pixels: m})
	if r.imageBytes+r.mathCache.bytes > decodedImageLimit || r.mathCache.get("math") != nil || r.ready["math"].pixels != nil {
		t.Fatal("thumbnail pressure retained unaccounted math references")
	}
	if r.accept(renderReply{key: "source", pixels: m, source: true}) {
		t.Fatal("source preview confused with viewport/cache result")
	}
	r.accept(renderReply{key: "bad", math: true, err: fmt.Errorf("unsupported formula")})
	if len(r.mathCache.entries) != 0 {
		t.Fatal("thumbnail/source/error entered formula cache")
	}
	if err := r.ensure(nil); err != nil || r.imageBytes != 0 {
		t.Fatal("nonvisible thumbnails retained", err, r.imageBytes)
	}
	// A cached raster remains separate from thumbnail identities even if the
	// caller accidentally asks for the same spelling of a formula's source path.
	key := assets.Key("mathjax-hires-v1", "fixture", "x", "16", fmt.Sprint(assets.MathRasterScale))
	if key == assets.Key("thumbnail-v1", "x") {
		t.Fatal("formula and image identities overlap")
	}
}

func TestOversizedVisibleFormulaSetSettlesUntilViewportChanges(t *testing.T) {
	r := mathCacheTestRenderer()
	rows := []line{{assets: []placedImage{{key: "a", tex: "a"}, {key: "b", tex: "b"}}}}
	if err := r.ensure(rows); err != nil || len(r.tasks) != 2 {
		t.Fatal("visible formulas were not initially queued", err, len(r.tasks))
	}
	m := countedRaster{image.Rect(0, 0, 2048, 2560)} // Two 20 MiB rasters cannot fit.
	for len(r.tasks) > 0 {
		task := <-r.tasks
		r.accept(renderReply{key: task.key, pixels: m, math: true})
	}
	if r.ready["a"].err == nil || r.ready["a"].pixels != nil || r.mathCache.bytes > decodedImageLimit {
		t.Fatal("evicted visible formula has no bounded fallback")
	}
	for range 10 {
		if err := r.ensure(rows); err != nil || len(r.tasks) != 0 {
			t.Fatal("visible working set continually rescheduled renders", err, len(r.tasks))
		}
	}
	if err := r.ensure([]line{{assets: []placedImage{{key: "b", tex: "b"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := r.ensure(rows); err != nil || len(r.tasks) != 1 {
		t.Fatal("viewport transition did not allow a fresh request", err, len(r.tasks))
	}
	if task := <-r.tasks; task.key != "a" {
		t.Fatal("rescheduled the surviving raster", task.key)
	}
}
