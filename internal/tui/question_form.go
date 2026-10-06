package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"ttc/internal/session"

	"github.com/gdamore/tcell/v2"
)

type questionAnswer struct {
	choice             int
	selected           int // Option index, or -1 when no option is selected.
	custom             composer
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
			d.editText(ev)
		}
		d.update()
		return nil, false
	}
	if d.textEntry() {
		a := &d.answers[d.tab]
		handled := true
		if ev.Key() == tcell.KeyEscape {
			a.editing = false
		} else if ev.Key() == tcell.KeyEnter && ev.Modifiers()&tcell.ModShift == 0 {
			a.editing = false
			d.tab++
			d.Window.Scroll = 0
		} else {
			handled = d.editText(ev)
		}
		if handled {
			d.manualScroll = false
			d.update()
			return nil, false
		}
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
			if !a.editing {
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

func (d *questionDialog) textEntry() bool {
	return d.tab < len(d.answers) && d.answers[d.tab].editing
}

// editText applies the shared editing or paste handler transactionally so a
// rejected insertion cannot change the answer's cursor or kill buffer.
func (d *questionDialog) editText(ev *tcell.EventKey) bool {
	next := d.answers[d.tab].custom
	handled := true
	if d.pasting {
		next.pasteKey(ev)
	} else {
		handled = next.key(ev)
	}
	if err := d.acceptText(d.tab, next); err != nil {
		d.errorText = err.Error()
	}
	return handled
}

func (d *questionDialog) acceptText(tab int, next composer) error {
	if len(next.text) > session.MaxAnswerBytes {
		return fmt.Errorf("Text is limited to %d bytes.", session.MaxAnswerBytes)
	}
	if !utf8.ValidString(next.text) {
		return fmt.Errorf("Text must be valid UTF-8.")
	}
	d.answers[tab].custom = next
	return nil
}

// values validates the whole round and points to the first missing answer.
func (d *questionDialog) values() ([]session.Answer, int) {
	values := make([]session.Answer, len(d.answers))
	for i, a := range d.answers {
		q := d.form.Questions[i]
		values[i].ID = q.ID
		if a.useCustom {
			if strings.TrimSpace(a.custom.text) == "" {
				return nil, i
			}
			values[i].Source, values[i].Values = "custom", []string{a.custom.text}
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
	d.Window.HideHint = true
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
		for i, a := range d.answers {
			q := d.form.Questions[i]
			answer := "Unanswered"
			if a.useCustom && strings.TrimSpace(a.custom.text) != "" {
				answer = a.custom.text
			} else if !a.useCustom && a.selected >= 0 {
				answer = q.Options[a.selected].Label
			}
			rows = append(rows, fmt.Sprintf("%d. %s\n   %s", i+1, strings.ReplaceAll(q.Prompt, "\n", "\n   "), strings.ReplaceAll(answer, "\n", "\n   ")), "")
		}
		d.focusRow = len(rows)
		rows = append(rows, "> [ Submit answers ]", "", "Review answers · Enter submits · ← returns to a question", "←/→ tabs · Esc dismisses")
	} else {
		q, a := d.form.Questions[d.tab], d.answers[d.tab]
		rows = append(rows, q.Prompt, "")
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
		if a.useCustom || a.custom.text != "" {
			text := a.custom.text
			if a.editing {
				runes := []rune(text)
				text = string(runes[:a.custom.cursor]) + "▏" + string(runes[a.custom.cursor:])
				d.focusRow = len(rows) + 1
			}
			rows = append(rows, "", "Text: "+text)
		}
		rows = append(rows, "")
		if a.editing {
			rows = append(rows, "Enter advances · Shift+Enter/Ctrl+J newline · Esc leaves text entry", "←/→ cursor · Tab/Shift+Tab tabs · Ctrl+X E editor")
		} else {
			rows = append(rows, "Up/Down choose · Enter selects and advances · Space selects", "←/→ tabs · Esc dismisses")
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
