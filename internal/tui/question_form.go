package tui

import (
	"fmt"
	"strings"
	"unicode"

	"scicode/internal/session"

	"github.com/gdamore/tcell/v2"
)

type questionAnswer struct {
	choice             int
	selected           int // Option index, or -1 when no option is selected.
	custom             []rune
	caret              int
	useCustom, editing bool
}

// questionDialog owns a round's editable answers. Only the final tab submits;
// its shared Window supplies a fixed tab header and scrollable question body.
type questionDialog struct {
	Window                Window
	form                  session.QuestionForm
	tab                   int
	answers               []questionAnswer
	errorText             string
	focusRow              int
	rows                  []string
	manualScroll, pasting bool
}

func newQuestionDialog(form session.QuestionForm) *questionDialog {
	d := &questionDialog{form: form, answers: make([]questionAnswer, len(form.Questions))}
	for i, q := range form.Questions {
		d.answers[i].selected = -1
		for j, o := range q.Options {
			if o.ID == q.RecommendedOptionID {
				d.answers[i].choice = j
			}
		}
		if len(q.Options) == 0 {
			d.answers[i].useCustom, d.answers[i].editing = true, true
		}
	}
	d.update()
	return d
}

// key returns answers only after an explicit Enter on Submit, or dismissed on
// Esc outside text entry. Switching tabs never loses draft text or selections.
func (d *questionDialog) key(ev *tcell.EventKey, height int) (answers []session.Answer, dismissed bool) {
	d.errorText = ""
	if d.pasting {
		if d.tab < len(d.answers) {
			switch ev.Key() {
			case tcell.KeyEnter:
				d.insert('\n')
			case tcell.KeyTab:
				d.insert('\t')
			case tcell.KeyRune:
				if !unicode.IsControl(ev.Rune()) {
					d.insert(ev.Rune())
				}
			}
		}
		d.update()
		return nil, false
	}
	switch ev.Key() {
	case tcell.KeyLeft, tcell.KeyBacktab:
		if d.tab > 0 {
			d.tab--
		}
		d.Window.Scroll = 0
	case tcell.KeyRight, tcell.KeyTab:
		if d.tab < len(d.answers) {
			d.tab++
		}
		d.Window.Scroll = 0
	case tcell.KeyCtrlU, tcell.KeyCtrlD, tcell.KeyPgUp, tcell.KeyPgDn:
		d.Window.Key(ev, height)
		d.manualScroll = true
		return nil, false
	default:
		if d.tab == len(d.answers) {
			if ev.Key() == tcell.KeyEscape {
				return nil, true
			}
			if ev.Key() == tcell.KeyEnter {
				answers, missing := d.values()
				if missing < 0 {
					return answers, false
				}
				d.tab = missing
				d.errorText = "Answer this question before submitting."
			}
		} else {
			q, a := d.form.Questions[d.tab], &d.answers[d.tab]
			if a.editing {
				switch ev.Key() {
				case tcell.KeyEscape:
					a.editing = false
				case tcell.KeyHome:
					a.caret = 0
				case tcell.KeyEnd:
					a.caret = len(a.custom)
				case tcell.KeyBackspace, tcell.KeyBackspace2:
					if a.caret > 0 {
						a.custom = append(a.custom[:a.caret-1], a.custom[a.caret:]...)
						a.caret--
					}
				case tcell.KeyDelete:
					if a.caret < len(a.custom) {
						a.custom = append(a.custom[:a.caret], a.custom[a.caret+1:]...)
					}
				case tcell.KeyEnter:
					if ev.Modifiers()&tcell.ModShift != 0 {
						d.insert('\n')
					} else {
						a.editing = false
					}
				case tcell.KeyRune:
					if !unicode.IsControl(ev.Rune()) {
						d.insert(ev.Rune())
					}
				}
			} else {
				switch ev.Key() {
				case tcell.KeyEscape:
					return nil, true
				case tcell.KeyUp:
					a.choice = max(0, a.choice-1)
				case tcell.KeyDown:
					a.choice = min(len(q.Options), a.choice+1)
				case tcell.KeyHome:
					a.choice = 0
				case tcell.KeyEnd:
					a.choice = len(q.Options)
				case tcell.KeyEnter, tcell.KeyRune:
					if ev.Key() == tcell.KeyEnter || ev.Rune() == ' ' {
						if a.choice == len(q.Options) {
							a.useCustom, a.editing = true, true
						} else {
							a.useCustom = false
							a.selected = a.choice
							if ev.Key() == tcell.KeyEnter {
								d.tab++
								d.Window.Scroll = 0
							}
						}
					}
				}
			}
		}
	}
	d.manualScroll = false
	d.update()
	return nil, false
}

func (d *questionDialog) insert(r rune) {
	a := &d.answers[d.tab]
	if len(string(a.custom))+len(string(r)) > session.MaxAnswerBytes {
		d.errorText = fmt.Sprintf("Text is limited to %d bytes.", session.MaxAnswerBytes)
		return
	}
	a.custom = append(a.custom, 0)
	copy(a.custom[a.caret+1:], a.custom[a.caret:])
	a.custom[a.caret] = r
	a.caret++
}

// values validates the whole round and points to the first missing answer.
func (d *questionDialog) values() ([]session.Answer, int) {
	values := make([]session.Answer, len(d.answers))
	for i, a := range d.answers {
		q := d.form.Questions[i]
		values[i].ID = q.ID
		if a.useCustom {
			if strings.TrimSpace(string(a.custom)) == "" {
				return nil, i
			}
			values[i].Source, values[i].Values = "custom", []string{string(a.custom)}
		} else {
			if a.selected < 0 {
				return nil, i
			}
			values[i].Source, values[i].Values = "option", []string{q.Options[a.selected].ID}
		}
	}
	return values, -1
}

func (d *questionDialog) update() {
	d.Window.Title = "Questions"
	d.Window.Hint = "←/→ tabs · Esc dismisses"
	var tabs []string
	for i := range d.form.Questions {
		label := fmt.Sprintf("%d", i+1)
		if i == d.tab {
			label = "[" + label + "]"
			d.Window.HeaderFocus = label
		}
		tabs = append(tabs, label)
	}
	label := "Submit"
	if d.tab == len(d.answers) {
		label = "[Submit]"
		d.Window.HeaderFocus = label
	}
	d.Window.Header = strings.Join(append(tabs, label), " | ")
	var rows []string
	if d.errorText != "" {
		rows = append(rows, "Error: "+d.errorText, "")
	}
	if d.tab == len(d.answers) {
		rows = append(rows, "Review answers. Left returns to a question.", "")
		for i, a := range d.answers {
			q := d.form.Questions[i]
			answer := "Unanswered"
			if a.useCustom && strings.TrimSpace(string(a.custom)) != "" {
				answer = string(a.custom)
			} else if !a.useCustom && a.selected >= 0 {
				answer = q.Options[a.selected].Label
			}
			rows = append(rows, q.Prompt+"\n  "+answer, "")
		}
		d.focusRow = len(rows)
		rows = append(rows, "> [ Submit answers ] · Enter")
	} else {
		q, a := d.form.Questions[d.tab], d.answers[d.tab]
		rows = append(rows, q.Prompt, "")
		hint := "Up/Down choose · Enter selects and advances · Space selects"
		rows = append(rows, hint, "")
		for i, o := range q.Options {
			mark := "( )"
			if a.selected == i && !a.useCustom {
				mark = "(x)"
			}
			text := mark + " " + o.Label
			if o.ID == q.RecommendedOptionID {
				text += " (Recommended)"
			}
			if o.Description != "" {
				text += "\n    " + o.Description
			}
			if i == a.choice {
				d.focusRow = len(rows)
			}
			rows = append(rows, menuRow(text, i == a.choice))
		}
		if a.choice == len(q.Options) {
			d.focusRow = len(rows)
		}
		mark := "( )"
		if a.useCustom {
			mark = "(x)"
		}
		rows = append(rows, menuRow(mark+" Other · free-text input", a.choice == len(q.Options)))
		if a.useCustom || len(a.custom) > 0 {
			text := string(a.custom)
			if a.editing {
				text = string(a.custom[:a.caret]) + "▏" + string(a.custom[a.caret:])
				d.focusRow = len(rows) + 1
			}
			rows = append(rows, "", "Text: "+text)
		}
		if a.editing {
			rows = append(rows, "Enter finishes text · Shift+Enter newline · ←/→ switches tabs")
		}
	}
	d.Window.Text = strings.Join(rows, "\n")
	d.rows = rows
}

func (d *questionDialog) reveal(width, height int) {
	if d.manualScroll || height <= 0 {
		return
	}
	position := 0
	for _, row := range d.rows[:d.focusRow] {
		position += len(wrap(row, width))
	}
	for i, row := range wrap(d.rows[d.focusRow], width) {
		if strings.Contains(row, "▏") {
			position += i
			break
		}
	}
	if position < d.Window.Scroll {
		d.Window.Scroll = position
	} else if position >= d.Window.Scroll+height {
		d.Window.Scroll = position - height + 1
	}
}
