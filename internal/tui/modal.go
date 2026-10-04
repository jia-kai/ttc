package tui

import "ttc/internal/session"

// modalState owns one foreground view and its asynchronous image load.
// Clearing it discards late source results without using window titles as state.
type modalState struct {
	window     *Window
	menu       *modelMenu
	commands   *commandMenu
	sessions   *sessionMenu
	history    *historyMenu
	prompts    *promptSearch
	background *backgroundMenu
	question   *questionDialog
	preview    *imagePreview
	loading    *previewLoad
	generation uint64
}
type previewLoad struct {
	snapshot   session.ImageSnapshot
	entryID    int64
	generation uint64
}

func (m *modalState) clear() { *m = modalState{} }

func (m *modalState) empty() bool {
	return m.window == nil && m.menu == nil && m.commands == nil && m.sessions == nil && m.history == nil && m.prompts == nil && m.background == nil && m.question == nil && m.preview == nil && m.loading == nil
}
