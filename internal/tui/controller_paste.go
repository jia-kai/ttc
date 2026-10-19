package tui

import (
	"strings"
	"unicode/utf8"

	"github.com/gdamore/tcell/v3"
)

// pasteBatch captures the destination at the opening bracket. Modal changes may
// discard remaining text, but must never redirect it to a different input.
// text contains accepted decoded chunks for one event-loop batch only.
type pasteBatch struct {
	active, composer bool
	question         *questionDialog
	tab              int
	search           *promptSearch
	text             strings.Builder
	searchChunks     []string
	questionTouched  bool
	questionError    string
}

func decodedPasteText(ev *tcell.EventKey) string {
	switch {
	case !ev.Pressed():
		return ""
	case ev.Key() == tcell.KeyEnter || isCtrl(ev, "j"):
		return "\n"
	case ev.Key() == tcell.KeyTab:
		return "\t"
	case ev.Key() == tcell.KeyRune && ev.Modifiers()&tcell.ModCtrl == 0:
		return ev.Str()
	default:
		return ""
	}
}

func (c *frontendController) collectPasteKey(ev *tcell.EventKey) {
	p := &c.paste
	text := decodedPasteText(ev)
	switch {
	case p.search != nil:
		if c.modal.prompts == p.search && text != "" {
			p.searchChunks = append(p.searchChunks, text)
		}
	case p.question != nil:
		if c.modal.question != p.question || p.tab >= len(p.question.answers) {
			return
		}
		p.questionTouched, p.questionError = true, ""
		// Validate each decoded chunk transactionally, as ordinary question
		// editing does. An oversized event must not consume later small events.
		bytes := len(p.question.answers[p.tab].custom.text) + p.text.Len() + len(text)
		if err := validateQuestionText(bytes, utf8.ValidString(text)); err != nil {
			p.questionError = err.Error()
		} else {
			p.text.WriteString(text)
		}
	case p.composer:
		c.modal.clear()
		p.text.WriteString(text)
	}
}

func (c *frontendController) flushPaste() {
	p := &c.paste
	if p.search != nil && c.modal.prompts == p.search && len(p.searchChunks) != 0 {
		m := p.search
		before := m.query.text
		// Search is bounded at 256 runes. Preserve its per-event truncation
		// and cursor semantics, while filtering the history only once.
		for _, text := range p.searchChunks {
			m.query.insert(text)
			m.limitQuery()
		}
		if m.query.text != before {
			m.filter()
		} else {
			m.update()
		}
	}
	if p.question != nil && c.modal.question == p.question && p.questionTouched {
		d := p.question
		if p.text.Len() != 0 {
			d.answers[p.tab].custom.insert(p.text.String())
		}
		d.errorText = p.questionError
		d.update()
	} else if p.composer && p.text.Len() != 0 {
		c.draft.insert(p.text.String())
	}
	p.text.Reset()
	clear(p.searchChunks) // Do not retain oversized decoded search events until paste-end.
	p.searchChunks = p.searchChunks[:0]
	p.questionTouched = false
	p.questionError = ""
}
