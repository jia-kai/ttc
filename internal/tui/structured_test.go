package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"ttc/internal/history"
)

func TestStructuredWindowPlainNarrowAndCachedMode(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	source := "Settings\n  words: [\"one\", \"界\", true]\n  text:\n    literal **Markdown**"
	w := &Window{Text: source, Structured: true}
	rows := w.Lines(12, 100)
	if w.Text != source {
		t.Fatal("source replaced with styling")
	}
	colored := false
	for _, row := range rows {
		if ansi.StringWidth(row) > 12 {
			t.Fatalf("row too wide: %q", row)
		}
		colored = colored || strings.Contains(row, "\x1b[")
	}
	if !colored {
		t.Fatal("missing structured highlighting")
	}
	// A mode-only change must invalidate cached rows, even with identical text.
	w.Structured = false
	if strings.Contains(strings.Join(w.Lines(12, 100), ""), "\x1b[") {
		t.Fatal("stale styled rows")
	}
	w.Structured = true
	t.Setenv("NO_COLOR", "1")
	if strings.Contains(strings.Join(w.Lines(12, 100), ""), "\x1b[") {
		t.Fatal("NO_COLOR not honored")
	}
}

func TestStructuredLongKeyFlowListWrapsAtTerminalCells(t *testing.T) {
	source := "Input\n  " + strings.Repeat("界", 27) + `: ["界界界界界界", "界界界界界界"]`
	w := &Window{Text: source, Structured: true}
	for _, width := range []int{12, 40, 80} {
		for _, row := range w.Lines(width, 100) {
			if ansi.StringWidth(row) > width {
				t.Fatalf("flow list overflowed %d terminal cells: %q", width, row)
			}
		}
	}
	if w.Text != source {
		t.Fatal("wrapping altered the source flow list")
	}
}

func TestStructuredPagedContinuationAndUnicodeOffsets(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	first := history.InspectionPage{Text: "Input\n  text: \"界", Total: 70, Limit: 16, Structured: true}
	second := history.InspectionPage{Text: "  fragment: true\n  number: 42", Offset: 16, Total: 70, Limit: 16, Structured: true}
	w := NewPagedWindow(first, func(offset int) (history.InspectionPage, error) {
		if offset != 16 {
			t.Fatalf("unexpected Unicode-character offset %d", offset)
		}
		return second, nil
	})
	w.Lines(80, 100)
	w.pageKey("]")
	rows := w.Lines(80, 100)
	if strings.Contains(rows[0], "\x1b[") {
		t.Fatal("fragment styled as complete source row", rows)
	}
	if !strings.Contains(rows[1], "\x1b[") {
		t.Fatal("complete row unstyled", rows)
	}
	if w.Text != second.Text {
		t.Fatal("paged text altered", w.Text)
	}
	// Switching from structured to legacy readable source must clear the mode.
	w.setPage(history.InspectionPage{Text: "legacy\n  key:\n    value", Limit: 16, Total: 28})
	if w.Structured || strings.Contains(strings.Join(w.Lines(80, 100), ""), "\x1b[") {
		t.Fatal("legacy compatibility changed")
	}
}
