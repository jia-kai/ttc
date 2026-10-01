package tui

// promptHistory recalls only human composer submissions from this frontend
// lifetime, including commands. Runtime notifications never enter this list.
type promptHistory struct {
	entries []string
	index   int
	draft   string
}

func (h *promptHistory) add(text string) {
	if len(h.entries) == 0 || h.entries[len(h.entries)-1] != text {
		h.entries = append(h.entries, text)
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
