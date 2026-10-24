package render

import (
	"strings"
	"testing"
)

func TestFormulaFittingLazyAndMemoized(t *testing.T) {
	calls := 0
	layout := func(Asset, int) FixedBox { calls++; return FixedBox{Rows: []string{"M"}, Width: 1} }
	items := make([]flowItem, 5000)
	for i := range items {
		items[i] = assetItem(layout, Asset{Kind: MathAsset, Source: "x"})
	}
	if calls != 0 {
		t.Fatal("eager fitting", calls)
	}
	for _, width := range []int{37, 37, 19, 19} {
		if got := strings.Join(layoutFlow(items, width), ""); got != strings.Repeat("M", len(items)) {
			t.Fatal("lost formulas")
		}
	}
	if calls != 2*len(items) {
		t.Fatalf("calls=%d; want %d", calls, 2*len(items))
	}
}

func BenchmarkASTAdjacentFormulas(b *testing.B) {
	for _, size := range []struct {
		name  string
		count int
	}{{"1k", 1000}, {"10k", 10000}} {
		b.Run(size.name, func(b *testing.B) {
			input := strings.Repeat("$x$", size.count)
			b.SetBytes(int64(len(input)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := TerminalWithAssets(input, 80, func(Asset, int) FixedBox { return FixedBox{Rows: []string{"M"}, Width: 1} }); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
