package render

// The shared dark-terminal palette uses light blue/cyan, lavender and amber.
// Statuses retain words/icons; red and green are never the only distinction.
const (
	TextColor     = "#dce4ec" // Primary text on dark terminal surfaces.
	MutedColor    = "#9eafc0" // Secondary text; kept readable rather than faint.
	BlueColor     = "#a7cfff" // Inline code, identifiers and links, without a background.
	CyanColor     = "#8bd5e8" // Headings, table headers and active controls.
	LavenderColor = "#c7b8f5" // System messages and syntax keywords.
	AmberColor    = "#f2c879" // Warnings/errors and deleted diff lines.
	SurfaceColor  = "#17212b" // Sidebar and inspector surface.
	HumanColor    = "#233448" // Subtle user-instruction/queued-message background.
	BorderColor   = "#72869b" // Borders and scrollbars on dark surfaces.
)
