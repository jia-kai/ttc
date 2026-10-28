package render

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkStructuredPage(b *testing.B) {
	for _, size := range []int{1024, 4096, 16384} {
		for _, complete := range []bool{false, true} {
			b.Run(fmt.Sprintf("string/%d/complete=%t", size, complete), func(b *testing.B) {
				text := "  text: \"" + strings.Repeat("a", size)
				if complete {
					text += "\""
				}
				b.ReportAllocs()
				b.SetBytes(int64(len(text)))
				for b.Loop() {
					StructuredPage(text, StructuredState{}, true)
				}
			})
		}
	}
}
