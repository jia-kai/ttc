package tui

import (
	"fmt"
	"strings"
	"testing"

	"ttc/internal/history"
)

func BenchmarkRequestInspectionWindow(b *testing.B) {
	b.Setenv("NO_COLOR", "")
	for _, fixture := range []struct{ name, row string }{
		{"fields", "  name: [\"hello\", 42, true, null]\n"},
		{"text", "  text: |\n    literal **Markdown** with 界 and terminal text\n"},
		{"long-line", "  text: \"" + strings.Repeat("a", history.InspectionPageChars) + "\""},
	} {
		row := []rune(fixture.row)
		text := string([]rune(strings.Repeat(fixture.row, history.InspectionPageChars/len(row)+1))[:history.InspectionPageChars])
		for _, cached := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/cached=%t", fixture.name, cached), func(b *testing.B) {
				page := history.InspectionPage{Text: text, Limit: history.InspectionPageChars, Total: 100 * history.InspectionPageChars, Structured: true}
				w := NewPagedWindow(page, nil)
				w.Lines(100, 30)
				b.ReportAllocs()
				b.SetBytes(int64(len(text)))
				b.ResetTimer()
				for b.Loop() {
					if !cached {
						w = NewPagedWindow(page, nil)
					}
					w.Lines(100, 30)
				}
			})
		}
	}
}
