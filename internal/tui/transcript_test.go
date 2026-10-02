package tui

import (
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"scicode/internal/render"
	"strings"
	"testing"
)

func transcriptOf(lines []line) *transcript {
	t := newTranscript()
	for _, v := range lines {
		t.append(v)
	}
	return t
}

func TestToolDiffBriefingHasShortHighlightedRows(t *testing.T) {
	text := "**write** · `sample.go`\n\n" + render.Fence("sample.go\n-old\n+new", "diff")
	for _, width := range []int{18, 80} {
		v := transcriptOf([]line{{text: text, id: 42, markdown: true, brief: true}})
		rows := v.viewport(width, 12)
		plain := ""
		for _, row := range rows {
			plain += row.text + "\n"
			if row.id != 42 {
				t.Fatal("lost inspection identity", row)
			}
		}
		if len(rows) < 3 || !strings.Contains(plain, "old") || !strings.Contains(plain, "new") {
			t.Fatal(rows)
		}
	}
}

func TestChunksPreserveMarkdownContainersAndLongIndentedCode(t *testing.T) {
	for _, source := range []string{
		"> ```python\n" + strings.Repeat("> result = 12345\n", 2200) + "> literal = '$x^2$'\n> ```\n\n$x^2$",
		"- ```python\n" + strings.Repeat("  result = 12345\n", 2200) + "  literal = '$x^2$'\n  ```\n\n$x^2$",
		"    " + strings.Repeat("code ", 8000) + "literal '$x^2$'\n\n$x^2$",
		"> ```python\n> " + strings.Repeat("code ", 8000) + "literal '$x^2$'\n> ```\n\n$x^2$",
	} {
		v := newTranscript()
		v.append(line{text: source, markdown: true})
		calls := 0
		v.layout = func(l line, w int) []displayRow {
			s := render.Math(l.text, func(tex string, block bool) string { calls++; return "FORMULA" })
			return rowsOf(layoutText(line{text: s, markdown: true}, w))
		}
		for i := range v.blocks {
			v.measure(i)
		}
		if calls != 1 {
			t.Fatal("code math escaped a container/chunk", source[:min(40, len(source))], calls)
		}
	}
}

func TestChunksPreserveInlineAndBlockMath(t *testing.T) {
	for _, expression := range []string{"$x^2 + y^2$", "$$\nx^2 + y^2\n$$", `\(x^2 + y^2\)`, `\[x^2 + y^2\]`} {
		source := strings.Repeat("a", (16<<10)-4) + expression + " rest"
		v := transcriptOf([]line{{text: source, markdown: true}})
		formulas := 0
		var reconstructed strings.Builder
		for _, b := range v.blocks {
			reconstructed.WriteString(b.text)
			if len(b.text) > 16<<10 {
				t.Fatal("formula changed chunk bound", len(b.text))
			}
			render.Math(b.text, func(tex string, block bool) string {
				formulas++
				return "formula"
			})
		}
		if formulas != 1 || reconstructed.String() != source {
			t.Fatal("formula split or source changed", expression, formulas)
		}
	}
}

func TestPageDownAtBottomResumesFollowing(t *testing.T) {
	v := newTranscript()
	for i := range 50 {
		v.append(line{text: fmt.Sprintf("line-%d", i)})
	}
	v.viewport(80, 5)
	v.scroll(-10, 5)
	v.viewport(80, 5)
	v.pageDown(5)
	if v.followTail {
		t.Fatal("page down away from bottom started following")
	}
	v.scroll(10000, 5)
	v.viewport(80, 5)
	if v.followTail {
		t.Fatal("wheel scrolling started following")
	}
	v.pageDown(5)
	v.append(line{text: "new streamed output"})
	rows := v.viewport(80, 5)
	if !v.followTail || rows[len(rows)-1].text != "new streamed output" {
		t.Fatal("page down at bottom did not reveal incoming output", rows)
	}
}

func TestTranscriptManualBottomAndSourceAnchor(t *testing.T) {
	v := newTranscript()
	var source strings.Builder
	for i := range 500 {
		fmt.Fprintf(&source, "word%03d ", i)
	}
	v.append(line{text: source.String(), id: 1})
	v.viewport(80, 5)
	v.scroll(-20, 5)
	rows := v.viewport(80, 5)
	word := strings.Fields(rows[0].text)[0]
	resized := v.viewport(40, 5)
	if !strings.Contains(resized[0].text, word) {
		t.Fatal("resize lost source position", word, resized[0].text)
	}
	v.scroll(10000, 5)
	v.viewport(40, 5)
	if v.followTail {
		t.Fatal("scrolling to bottom resumed following")
	}
	before := v.firstLine
	v.append(line{text: "new output", id: 2})
	v.viewport(40, 5)
	if v.firstLine != before {
		t.Fatal("manual bottom moved on append")
	}
}

func TestTranscriptChunksCarryCodeFence(t *testing.T) {
	for _, fence := range []string{"```python", "~~~python"} {
		v := newTranscript()
		v.append(line{text: fence + "\n" + strings.Repeat("result = 12345\n", 2200) + "literal = '$x^2$'\n" + fence[:3] + "\n\n$x^2$", markdown: true})
		calls := 0
		v.layout = func(l line, w int) []displayRow {
			s := render.Math(l.text, func(tex string, block bool) string { calls++; return "FORMULA" })
			return rowsOf(layoutText(line{text: s, markdown: true}, w))
		}
		var rendered strings.Builder
		for i := range v.blocks {
			v.measure(i)
			for _, row := range v.blocks[i].rows {
				rendered.WriteString(ansi.Strip(row.text))
			}
		}
		if calls != 1 || !strings.Contains(rendered.String(), "$x^2$") {
			t.Fatal("code turned into math across chunks", calls)
		}
	}
}
func TestTranscriptPressureScrollAndResize(t *testing.T) {
	v := newTranscript()
	for i := range 100000 {
		v.append(line{text: fmt.Sprintf("message %d", i), id: int64(i + 1), markdown: true, brief: true})
	}
	v.layout = func(l line, w int) []displayRow { return rowsOf([]string{l.text}) }
	rows := v.viewport(80, 30)
	if len(rows) != 30 || rows[29].id != 100000 {
		t.Fatal(rows)
	}
	for range 100 {
		v.scroll(-3, 30)
		v.viewport(80, 30)
	}
	anchor := v.entryAt(0)
	v.viewport(40, 30)
	if v.entryAt(0) != anchor {
		t.Fatal("resize lost reading anchor")
	}
	if v.renders > 400 {
		t.Fatal("full history rendered", v.renders)
	}
	before := v.renders
	for range 100 {
		v.viewport(40, 30)
	}
	if v.renders != before {
		t.Fatal("idle frames relaid out history")
	}
}

func TestLongStreamingReplyPreservesHistoricalIndex(t *testing.T) {
	v := newTranscript()
	for i := range 100000 {
		v.append(line{text: fmt.Sprintf("message %d", i), brief: true})
	}
	v.layout = func(l line, w int) []displayRow { return rowsOf(wrap(l.text, w)) }
	v.assistant(1, "assistant", strings.Repeat("streaming ", 2200), 0, false)
	// Give tail growth space so a legitimate slice reallocation cannot hide
	// whether the update discarded/rebuilt the historical index.
	length := len(v.tree)
	v.tree = append(v.tree, make([]int, 16)...)
	v.tree = v.tree[:length]
	firstNode := &v.tree[1]
	prefix := v.prefix(100000)
	for range 256 {
		v.assistant(1, "assistant", strings.Repeat("new ", 16), 0, false)
		v.viewport(80, 25)
		if &v.tree[1] != firstNode || v.prefix(100000) != prefix {
			t.Fatal("streaming rebuilt or changed the historical index")
		}
	}
	if v.renders > 512 {
		t.Fatal("streaming rendered historical blocks", v.renders)
	}
}
func TestTranscriptHugeMessageHasBoundedLayoutChunks(t *testing.T) {
	v := newTranscript()
	v.append(line{text: strings.Repeat("long line of research results\n", 100000), id: 1, markdown: true})
	bytes := 0
	v.layout = func(l line, w int) []displayRow {
		if len(l.text) > 16<<10 {
			t.Fatal("unbounded block")
		}
		bytes += len(l.text)
		return rowsOf(wrap(l.text, w))
	}
	v.viewport(80, 30)
	if bytes > 32<<10 {
		t.Fatal("whole huge message rendered", bytes)
	}
	v.scroll(-10, 30)
	v.viewport(40, 30)
	if bytes > 64<<10 {
		t.Fatal(bytes)
	}
}
func TestTranscriptIndexMutationAndAnchors(t *testing.T) {
	v := transcriptOf([]line{{text: "a", id: 1}, {text: "b", id: 2}, {text: "c", id: 3}})
	v.viewport(30, 2)
	v.scroll(-1, 2)
	v.viewport(30, 2)
	if v.entryAt(0) != 1 {
		t.Fatal(v.visible)
	}
	v.replace(1, line{text: "b\nsecond\nthird", id: 2})
	v.viewport(30, 2)
	if v.entryAt(0) != 1 {
		t.Fatal("replacement moved anchor")
	}
	v.append(line{text: "d", id: 4})
	if v.prefix(len(v.blocks)) != 6 {
		t.Fatal(v.tree)
	}
}

func TestTranscriptReplacementPreservesLaterAnchorAndCache(t *testing.T) {
	v := transcriptOf([]line{{text: strings.Repeat("earlier output\n", 1300), id: 1}, {text: "later anchored message", id: 2}})
	v.viewport(80, 1)
	v.scroll(0, 1)
	for _, repeats := range []int{2600, 1300, 10} {
		v.replace(0, line{text: strings.Repeat("earlier output\n", repeats), id: 1})
		rows := v.viewport(80, 1)
		if len(rows) != 1 || rows[0].id != 2 || rows[0].text != "later anchored message" {
			t.Fatal("replacement moved a later reading anchor", rows)
		}
		for _, index := range v.recent {
			if index < 0 || index >= len(v.blocks) || v.blocks[index].rows == nil {
				t.Fatal("replacement left a stale cache index", index, v.recent)
			}
		}
		v.invalidate()
		if v.blocks[v.anchor].rows != nil {
			t.Fatal("invalidation missed shifted cached rows")
		}
	}
}

func TestTranscriptReplacementPreservesSourceWithinMessage(t *testing.T) {
	var source strings.Builder
	for i := range 2500 {
		fmt.Fprintf(&source, "source line %04d\n", i)
	}
	v := transcriptOf([]line{{text: source.String(), id: 1}})
	v.viewport(80, 1)
	v.scroll(-20, 1)
	before := v.viewport(80, 1)[0].text
	updated := source.String() + strings.Repeat("more streamed output\n", 1200)
	v.replace(0, line{text: updated, id: 1})
	rows := v.viewport(80, 1)
	if rows[0].text != before {
		t.Fatal("replacement lost its source anchor", before, rows[0].text)
	}
}
func BenchmarkTranscriptViewportLongHistory(b *testing.B) {
	v := newTranscript()
	for range 100000 {
		v.append(line{text: "message", brief: true})
	}
	v.layout = func(l line, w int) []displayRow { return rowsOf([]string{l.text}) }
	v.viewport(80, 40)
	b.ResetTimer()
	for range b.N {
		v.viewport(80, 40)
	}
}
