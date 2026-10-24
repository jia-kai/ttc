package render

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func imageFixture(asset Asset, width int) FixedBox {
	if asset.Kind != ImageAsset {
		return LiteralAsset(asset, width)
	}
	return FixedBox{Rows: []string{"III", "III", "III"}, Width: 3, Baseline: 1}
}

func TestImageBoxesUseInlineBaselinesAndContainerPrefixes(t *testing.T) {
	for _, test := range []struct{ source, want string }{
		{"before ![plot](plot.png) after", "       III      \nbefore III after\n       III      "},
		{"- before ![plot](plot.png) after", "         III      \n• before III after\n         III      "},
		{"> before ![plot](plot.png) after", "│        III      \n│ before III after\n│        III      "},
	} {
		out, err := TerminalWithAssets(test.source, 40, imageFixture)
		if err != nil {
			t.Fatal(err)
		}
		if got := ansi.Strip(out); got != test.want {
			t.Fatalf("%q: got %q; want %q", test.source, got, test.want)
		}
	}
	out, err := TerminalWithAssets("![plot](plot.png)", 2, imageFixture)
	if err != nil {
		t.Fatal(err)
	}
	if out != "III\nIII\nIII" {
		t.Fatalf("atomic image clipped or split: %q", out)
	}
}

func TestImageAndMathBoxesShareTableLayout(t *testing.T) {
	for _, width := range []int{2, 8, 24, 80} {
		imageSource := "> - Table:\n>\n>   | Asset | Text |\n>   |---|---|\n>   | ![plot](plot.png) | end |"
		mathSource := strings.ReplaceAll(imageSource, "![plot](plot.png)", "$x$")
		fit := func(asset Asset, width int) FixedBox {
			return FixedBox{Rows: []string{"III", "III", "III"}, Width: 3, Baseline: 1}
		}
		image, err := TerminalWithAssets(imageSource, width, fit)
		if err != nil {
			t.Fatal(err)
		}
		math, err := TerminalWithAssets(mathSource, width, fit)
		if err != nil {
			t.Fatal(err)
		}
		if image != math {
			t.Fatalf("width %d: image=%q math=%q", width, image, math)
		}
		if strings.Count(ansi.Strip(image), "III") != 3 {
			t.Fatalf("width %d lost image: %q", width, image)
		}
	}
}

func TestImageAssetSourceAndLiteralAlt(t *testing.T) {
	var assets []Asset
	source := `![**A** $x$ \(y\) &amp; &#27;](local&#x2f;plot.png?x=1&amp;y=2) and $z$`
	_, err := TerminalWithAssets(source, 80, func(asset Asset, width int) FixedBox {
		assets = append(assets, asset)
		return FixedBox{Rows: []string{"M"}, Width: 1}
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Asset{
		{Kind: ImageAsset, Source: "local/plot.png?x=1&y=2", Alt: `A $x$ \(y\) & `},
		{Kind: MathAsset, Source: "z"},
	}
	if !reflect.DeepEqual(assets, want) {
		t.Fatalf("assets=%#v; want %#v", assets, want)
	}
}

func TestLiteralAssetFallbackDoesNotLoadReferences(t *testing.T) {
	for _, source := range []string{"missing/local.png", "https://example.invalid/image.png"} {
		input := "![a $x$](" + source + ")"
		out, err := Terminal(input, 100)
		if err != nil {
			t.Fatal(err)
		}
		want := "Image: a $x$ → " + source
		if got := ansi.Strip(out); got != want {
			t.Fatalf("got %q; want %q", got, want)
		}
	}
	box := LiteralAsset(Asset{Kind: ImageAsset, Source: "a\x1b[2J.png", Alt: "safe\u202e"}, 80)
	plain := ansi.Strip(strings.Join(box.Rows, "\n"))
	if plain != "Image: safe → a[2J.png" {
		t.Fatalf("unsafe literal asset: %q", plain)
	}
}

func TestAssetFittingMemoizesImagesAndMath(t *testing.T) {
	for _, kind := range []AssetKind{MathAsset, ImageAsset} {
		calls := 0
		item := assetItem(func(asset Asset, width int) FixedBox {
			calls++
			return FixedBox{Rows: []string{"M"}, Width: 1}
		}, Asset{Kind: kind, Source: "value"})
		if calls != 0 {
			t.Fatal("eager asset fitting")
		}
		for _, width := range []int{0, 1, 8, 8, 1} {
			item.box(width)
		}
		if calls != 2 {
			t.Fatalf("kind %v callbacks=%d; want 2", kind, calls)
		}
	}
}
