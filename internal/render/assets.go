package render

import "github.com/charmbracelet/x/ansi"

// AssetKind identifies content whose terminal rectangle is supplied by a frontend.
type AssetKind uint8

const (
	// MathAsset contains TeX, without its Markdown delimiters.
	MathAsset AssetKind = iota + 1
	// ImageAsset contains an image reference; resolving or loading it belongs to the frontend.
	ImageAsset
)

// Asset is control-safe Markdown content passed to an AssetLayout. Source is
// container-normalized TeX for math, or the decoded literal destination for an
// image. Display identifies display math; images are inline boxes. Alt is an
// image's plain literal label, with no formula interpretation.
type Asset struct {
	Kind    AssetKind
	Source  string
	Display bool
	Alt     string
}

// AssetLayout fits an asset to width terminal columns. The returned rectangle
// is indivisible to surrounding text. Calls may include measurement passes;
// callers must not start asynchronous work until resulting rows intersect the
// viewport. Each occurrence and width is memoized within one document render.
type AssetLayout func(asset Asset, width int) FixedBox

func assetItem(layout AssetLayout, asset Asset) flowItem {
	cache := make(map[int]FixedBox)
	return flowItem{block: asset.Kind == MathAsset && asset.Display, box: func(width int) FixedBox {
		width = max(1, width)
		if box, ok := cache[width]; ok {
			return box
		}
		box := layout(asset, width)
		cache[width] = box
		return box
	}}
}

// LiteralAsset lays out readable TeX or an image's alt text and destination.
// It performs no filesystem or network I/O and can be used by graphics frontends
// when a reference is unavailable or unsupported. Image text uses Markdown's
// configured image styles; all supplied text is sanitized before presentation.
func LiteralAsset(asset Asset, width int) FixedBox {
	if asset.Kind != ImageAsset {
		return LiteralBox(asset.Source, width)
	}
	style := markdownStyle()
	items := []flowItem{{text: styled("Image: "+Clean(asset.Alt)+" →", style.ImageText)}}
	if asset.Source != "" {
		items = append(items, flowItem{text: styled(" "+Clean(asset.Source), style.Image)})
	}
	rows := layoutFlow(items, max(1, width))
	columns := 0
	for _, row := range rows {
		columns = max(columns, ansi.StringWidth(row))
	}
	return FixedBox{Rows: rows, Width: columns, Baseline: max(0, len(rows)-1)}
}
