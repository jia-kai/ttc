package tui

import "ttc/internal/history"

// promptHistory recalls saved human prompts and submissions from this frontend
// lifetime, including commands. Runtime notifications never enter this list.
type promptHistory struct {
	entries []string
	index   int
	draft   string
	bytes   int
}

func (h *promptHistory) add(text string) {
	if len(text) <= history.MaxPromptHistoryBytes && (len(h.entries) == 0 || h.entries[len(h.entries)-1] != text) {
		for len(h.entries) > 0 && (len(h.entries) >= history.MaxPromptHistoryEntries || h.bytes+len(text) > history.MaxPromptHistoryBytes) {
			h.bytes -= len(h.entries[0])
			copy(h.entries, h.entries[1:])
			h.entries[len(h.entries)-1] = ""
			h.entries = h.entries[:len(h.entries)-1]
		}
		h.entries = append(h.entries, text)
		h.bytes += len(text)
	}
	h.index = len(h.entries)
	h.draft = ""
}

func (h *promptHistory) move(current string, direction int) string {
	if len(h.entries) == 0 {
		return current
	}
	if h.index == len(h.entries) {
		h.draft = current
	}
	h.index = max(0, min(len(h.entries), h.index+direction))
	if h.index == len(h.entries) {
		return h.draft
	}
	return h.entries[h.index]
}
