package tui

// modalState owns one foreground view and its asynchronous image load.
// Clearing it discards late source results without using window titles as state.
type modalState struct {
	window        *Window
	menu          *modelMenu
	commands      *commandMenu
	sessions      *sessionMenu
	history       *historyMenu
	prompts       *promptSearch
	background    *backgroundMenu
	question      *questionDialog
	preview       *imagePreview
	loading       *previewLoad
	raw           *rawBoundaryState
	requestHeader int64 // Live request row whose readable inspector is open; zero for snapshots.
	expanded      bool  // Full readable request view; raw captures have their own mode.
	generation    uint64
	parent        *modalState // Prior view while a local Markdown image is enlarged.
}

type rawBoundaryState struct {
	header, capture int64 // Group revision snapshot and currently selected immutable capture.
}
type previewLoad struct {
	*imageLoad
	actor      string
	entryID    int64
	generation uint64
}

func (m *modalState) clear() {
	if m.loading != nil {
		m.loading.cancel()
	}
	*m = modalState{}
}

func (m *modalState) empty() bool {
	return m.window == nil && m.menu == nil && m.commands == nil && m.sessions == nil && m.history == nil && m.prompts == nil && m.background == nil && m.question == nil && m.preview == nil && m.loading == nil
}
