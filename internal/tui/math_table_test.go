package tui

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"ttc/internal/assets"
	"ttc/internal/graphics"
)

// These rasters have known logical sizes, independent of MathJax or the host's
// fonts. In particular, tall is four terminal rows at its natural width.
func tableMathRenderer() *imageRenderer {
	r := &imageRenderer{
		graphics: graphics.New(io.Discard, false), ctx: context.Background(),
		backend: "table-fixture", cellWidth: 8, cellHeight: 16,
		ready: map[string]renderReply{}, pending: map[string]context.CancelFunc{},
		tasks: make(chan renderTask, 32),
	}
	for tex, size := range map[string]image.Point{"tall": {64, 64}, "x": {24, 16}, "wide": {240, 64}} {
		key := tableMathKey(tex)
		r.ready[key] = renderReply{key: key, math: true, pixels: image.NewRGBA(image.Rect(0, 0, size.X*assets.MathRasterScale, size.Y*assets.MathRasterScale))}
	}
	return r
}

func TestMathTableNarrowLiteralFallbackIsCompleteAndViewportOnly(t *testing.T) {
	const tex = `\frac{alpha + beta}{gamma + delta}`
	const source = "| Formula | Note |\n| --- | --- |\n| $" + tex + "$ | visible |\n| $offscreen$ | hidden |\n| following | intact |"
	for _, mode := range []string{"pending", "formula-error", "backend-error"} {
		t.Run(mode, func(t *testing.T) {
			r := tableMathRenderer()
			r.ready = map[string]renderReply{}
			if mode == "formula-error" {
				r.ready[tableMathKey(tex)] = renderReply{math: true, err: errors.New("unsupported formula")}
			} else if mode == "backend-error" {
				r.backendError = errors.New("backend unavailable")
			}
			rows := r.layout(line{text: source, markdown: true}, 12)
			checkTableMathRows(t, r, rows, 12)
			var literal strings.Builder
			var visible []line
			for _, row := range rows {
				for _, p := range row.assets {
					if p.tex == tex {
						// Read only the formula cell, excluding the neighboring
						// column and the table's separator/padding whitespace.
						literal.WriteString(strings.Split(ansi.Strip(row.text), "│")[0])
						visible = append(visible, line{text: row.text, assets: row.assets})
					}
				}
			}
			want := tex
			if mode != "pending" {
				want = "[math unavailable] " + want
			}
			compact := func(text string) string { return strings.Join(strings.Fields(text), "") }
			if got := compact(literal.String()); got != compact(want) {
				t.Fatalf("narrow %s fallback lost TeX/label: got %q, want %q\n%s", mode, got, compact(want), tablePlainRows(rows))
			}
			if len(visible) < 2 {
				t.Fatal("fixture did not constrain the formula to multiple rows")
			}
			if len(r.tasks) != 0 {
				t.Fatal("layout requested work before the viewport was known")
			}
			for range 3 {
				if err := r.ensure(visible); err != nil {
					t.Fatal(err)
				}
			}
			wantTasks := 0
			if mode == "pending" {
				wantTasks = 1
			}
			if len(r.tasks) != wantTasks {
				t.Fatalf("fallback retried or requested nonvisible formulas: got %d tasks, want %d", len(r.tasks), wantTasks)
			}
			if wantTasks == 1 {
				task := <-r.tasks
				if task.tex != tex || len(r.pending) != 1 {
					t.Fatalf("requested formula outside viewport: task=%q pending=%d", task.tex, len(r.pending))
				}
				if !r.accept(renderReply{key: task.key, math: true, err: errors.New("unsupported formula")}) {
					t.Fatal("discarded visible formula error")
				}
				for range 3 {
					if err := r.ensure(visible); err != nil {
						t.Fatal(err)
					}
				}
				if len(r.tasks) != 0 || len(r.pending) != 0 {
					t.Fatal("permanent formula error caused a request loop")
				}
			}
		})
	}
}

func TestMathEntityDoesNotCollideWithImageRowToken(t *testing.T) {
	r := tableMathRenderer()
	rows := r.layout(line{text: "before &#xF0000; $x$ after", markdown: true}, 80)
	checkTableMathRows(t, r, rows, 80)
	plain := tablePlainRows(rows)
	if strings.Count(plain, "\U000f0000") != 1 || strings.Contains(plain, "&#xF0000;") {
		t.Fatalf("entity-encoded supplementary private-use text was consumed by image restoration: %q", plain)
	}
	placements := 0
	for _, row := range rows {
		for _, p := range row.assets {
			placements++
			if p.tex != "x" || p.rows != 1 {
				t.Fatalf("unexpected formula placement: %+v", p)
			}
		}
	}
	if placements != 1 || strings.Count(plain, "\U0010eeee") != 3 {
		t.Fatalf("literal entity became an extra image: placements=%d, text=%q", placements, plain)
	}
}

func tableMathKey(tex string) string {
	return assets.Key("mathjax-hires-v1", "table-fixture", tex, "16", fmt.Sprint(assets.MathRasterScale))
}

func tablePlainRows(rows []displayRow) string {
	var b strings.Builder
	for _, row := range rows {
		b.WriteString(ansi.Strip(row.text))
		b.WriteByte('\n')
	}
	return b.String()
}

// Inspect final rows rather than callbacks: measurement can invoke a formula
// factory more often than the final layout actually uses its result.
func checkTableMathRows(t *testing.T, r *imageRenderer, rows []displayRow, width int) {
	t.Helper()
	coordinateRows := map[string]int{}
	for y, row := range rows {
		plain := ansi.Strip(row.text)
		if ansi.StringWidth(row.text) > width {
			t.Fatalf("row %d exceeds width %d: %q", y, width, plain)
		}
		for _, ch := range plain {
			if ch >= 0xe000 && ch <= 0xf8ff {
				t.Fatalf("row %d leaked layout token %U: %q", y, ch, plain)
			}
		}
		cells := 0
		for _, p := range row.assets {
			if p.columns < 1 || p.columns > width || p.rows < 1 {
				t.Fatalf("invalid final placement on row %d: %+v", y, p)
			}
			if r.ready[p.key].pixels == nil {
				continue // Pending formulas carry requests, not Kitty coordinates.
			}
			grid, err := r.graphics.Rows(p.key, p.columns, p.rows)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			placement := fmt.Sprintf("%s:%dx%d", p.key, p.columns, p.rows)
			for imageY, candidate := range grid {
				if strings.Contains(plain, ansi.Strip(candidate)) {
					if want := coordinateRows[placement] % p.rows; imageY != want {
						t.Fatalf("row %d has image coordinate row %d, want %d", y, imageY, want)
					}
					coordinateRows[placement] = (coordinateRows[placement] + 1) % p.rows
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("row %d has unused asset or broken Kitty coordinates: %+v, %q", y, p, plain)
			}
			cells += p.columns
		}
		if got := strings.Count(plain, "\U0010eeee"); got != cells {
			t.Fatalf("row %d has %d placeholder cells, final assets account for %d: %q", y, got, cells, plain)
		}
	}
	for placement, next := range coordinateRows {
		if next != 0 {
			t.Fatalf("incomplete formula %s: next coordinate row %d", placement, next)
		}
	}
}

func tableSeparatorColumns(text string, separator rune) []int {
	plain := ansi.Strip(text)
	var columns []int
	for offset, ch := range plain {
		if ch == separator {
			columns = append(columns, ansi.StringWidth(plain[:offset]))
		}
	}
	return columns
}

func TestTallInlineMathKeepsProseOnBaseline(t *testing.T) {
	for _, source := range []string{
		"before $tall$ after",
		"**before** $tall$ _after_ with `code`",
		"> before $tall$ after",
		"- before $tall$ after",
	} {
		t.Run(source, func(t *testing.T) {
			r := tableMathRenderer()
			rows := r.layout(line{text: source, markdown: true}, 80)
			checkTableMathRows(t, r, rows, 80)
			imageRows, baseline := 0, false
			for _, row := range rows {
				if len(row.assets) > 0 {
					imageRows++
					plain := ansi.Strip(row.text)
					baseline = baseline || (strings.Contains(plain, "before") && strings.Contains(plain, "after"))
				}
			}
			if imageRows != 4 || !baseline {
				t.Fatalf("tall formula was split or promoted away from prose: %s", tablePlainRows(rows))
			}
		})
	}
}

const tableMathSource = "Before table.\n\n" +
	"| **Header** $tall$ | Meaning and units |\n" +
	"| :--- | ---: |\n" +
	"| Before $tall$ then $x$ after | **bold** _emphasis_ `code` [notes](notes.md) 界 |\n" +
	"| $$wide$$ and $x$ | second row |\n" +
	"| following | intact |\n\nAfter table."

func TestMathTableMultirowHeaderAndCells(t *testing.T) {
	for _, width := range []int{80, 38, 14, 2} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			r := tableMathRenderer()
			rows := r.layout(line{text: tableMathSource, markdown: true}, width)
			checkTableMathRows(t, r, rows, width)
			plain := tablePlainRows(rows)
			// Wrapping may split words, but must not lose any subsequent rows.
			var columns [2]strings.Builder
			for _, row := range rows {
				for i, cell := range strings.Split(ansi.Strip(row.text), "│") {
					if i < len(columns) {
						columns[i].WriteString(cell)
					}
				}
			}
			compact := strings.NewReplacer(" ", "", "─", "", "┼", "").Replace(columns[0].String() + columns[1].String())
			for _, want := range []string{"following", "intact", "Aftertable."} {
				if !strings.Contains(compact, want) {
					t.Fatalf("width %d lost %q after tall cells: %s", width, want, plain)
				}
			}
			if width < 38 {
				return // Very narrow tables may deliberately stack their cells.
			}
			var separators []int
			separatorRow, firstImage, lastImage := -1, -1, -1
			for i, row := range rows {
				if cols := tableSeparatorColumns(row.text, '┼'); len(cols) > 0 {
					separatorRow, separators = i, cols
				}
				if len(row.assets) > 0 {
					if firstImage < 0 {
						firstImage = i
					}
					lastImage = i
				}
			}
			if separatorRow <= firstImage || lastImage <= separatorRow {
				t.Fatalf("header/body math did not bracket table rule: %s", plain)
			}
			for i := firstImage; i <= lastImage+1 && i < len(rows); i++ {
				if i != separatorRow {
					if cols := tableSeparatorColumns(rows[i].text, '│'); !reflect.DeepEqual(cols, separators) {
						t.Fatalf("row %d separators %v, want %v: %s", i, cols, separators, plain)
					}
				}
			}
			if width == 80 {
				for _, want := range []string{"Header", "bold", "emphasis", "code", "notes", "界", "second", "row"} {
					if !strings.Contains(plain, want) {
						t.Fatalf("lost formatted cell content %q: %s", want, plain)
					}
				}
			}
		})
	}
}

func TestMathTablePendingReadyAndResize(t *testing.T) {
	r := tableMathRenderer()
	replies := r.ready
	r.ready = map[string]renderReply{}
	pending := r.layout(line{text: tableMathSource, markdown: true}, 80)
	checkTableMathRows(t, r, pending, 80)
	var visible []line
	for _, row := range pending {
		visible = append(visible, line{text: row.text, assets: row.assets})
	}
	if err := r.ensure(visible); err != nil {
		t.Fatal(err)
	}
	if len(r.tasks) != 3 {
		t.Fatalf("requested speculative/duplicate formulas: got %d tasks, want 3", len(r.tasks))
	}
	for len(r.tasks) > 0 {
		task := <-r.tasks
		if !r.accept(replies[task.key]) {
			t.Fatalf("discarded visible formula %q", task.tex)
		}
	}
	ready := r.layout(line{text: tableMathSource, markdown: true}, 80)
	checkTableMathRows(t, r, ready, 80)
	if len(ready) <= len(pending) {
		t.Fatalf("ready tall cells failed to reflow table: pending=%d ready=%d", len(pending), len(ready))
	}
	wideColumns, narrowColumns := 0, 0
	for _, row := range ready {
		for _, p := range row.assets {
			if p.tex == "wide" {
				wideColumns = max(wideColumns, p.columns)
			}
		}
	}
	narrow := r.layout(line{text: tableMathSource, markdown: true}, 24)
	checkTableMathRows(t, r, narrow, 24)
	for _, row := range narrow {
		for _, p := range row.assets {
			if p.tex == "wide" {
				narrowColumns = max(narrowColumns, p.columns)
			}
		}
	}
	if narrowColumns == 0 || narrowColumns >= wideColumns {
		t.Fatalf("formula did not fit actual resized cell: wide=%d narrow=%d", wideColumns, narrowColumns)
	}
	restored := r.layout(line{text: tableMathSource, markdown: true}, 80)
	if tablePlainRows(restored) != tablePlainRows(ready) {
		t.Fatal("resize changed cached raster coordinates or lost table content")
	}
	// A viewport containing only the following text must not inherit assets
	// encountered during table measurement or on earlier physical rows.
	for _, row := range restored {
		if strings.Contains(ansi.Strip(row.text), "following") || strings.Contains(ansi.Strip(row.text), "After table.") {
			if len(row.assets) != 0 {
				t.Fatalf("plain subsequent row inherited speculative assets: %+v", row.assets)
			}
		}
	}
}
