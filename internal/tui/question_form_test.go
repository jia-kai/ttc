package tui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/session"
)

func questionFixture() session.QuestionForm {
	options := []session.Option{{ID: "a", Label: "First"}, {ID: "b", Label: "Second", Description: "Useful explanation"}}
	return session.QuestionForm{ID: "form", Questions: []session.Question{
		{ID: "method", Prompt: "Which method?", Options: options, RecommendedOptionID: "b"},
		{ID: "notes", Prompt: "Any notes?", Options: options},
		{ID: "checks", Prompt: "Which check?", Options: options},
	}}
}

func TestQuestionTabsPreserveAnswersAndRequireFinalSubmit(t *testing.T) {
	d := newQuestionDialog(questionFixture())
	key := func(k tcell.Key) ([]session.Answer, bool) { return d.key(tcell.NewEventKey(k, 0, 0), 8) }
	if !strings.Contains(d.Window.Text, "> ( ) Second (Recommended)") {
		t.Fatal(d.Window.Text)
	}
	if _, missing := d.values(); missing != 0 {
		t.Fatal("recommendation answered automatically")
	}
	key(tcell.KeyRight)
	key(tcell.KeyRight)
	key(tcell.KeyRight)
	if !strings.Contains(d.Window.Header, "[Submit]") || !strings.Contains(d.Window.Text, "[ Submit answers ]") {
		t.Fatal(d.Window)
	}
	if answers, _ := key(tcell.KeyEnter); answers != nil || d.tab != 0 {
		t.Fatal("submitted missing answers", answers, d.tab)
	}
	if answers, _ := key(tcell.KeyEnter); answers != nil || d.tab != 1 {
		t.Fatal("choice did not advance without submitting", answers, d.tab)
	}
	key(tcell.KeyEnd)
	key(tcell.KeyEnter)
	for _, r := range "Looks good 界" {
		d.key(tcell.NewEventKey(tcell.KeyRune, r, 0), 8)
	}
	key(tcell.KeyRight)
	d.key(tcell.NewEventKey(tcell.KeyRune, ' ', 0), 8)
	key(tcell.KeyDown)
	d.key(tcell.NewEventKey(tcell.KeyRune, ' ', 0), 8)
	d.key(tcell.NewEventKey(tcell.KeyRune, ' ', 0), 8)
	if d.tab != 2 || d.answers[2].selected != 1 {
		t.Fatal("Space must replace the selection idempotently without advancing", d.tab, d.answers[2])
	}
	key(tcell.KeyLeft)
	if string(d.answers[1].custom) != "Looks good 界" || !d.answers[1].editing {
		t.Fatal("text lost across tabs", d.answers[1])
	}
	key(tcell.KeyRight)
	if answers, _ := key(tcell.KeyEnter); answers != nil || d.tab != 3 {
		t.Fatal("last question must advance to Submit without sending", answers, d.tab)
	}
	answers, closed := key(tcell.KeyEnter)
	want := []session.Answer{{ID: "method", Values: []string{"b"}, Source: "option"}, {ID: "notes", Values: []string{"Looks good 界"}, Source: "custom"}, {ID: "checks", Values: []string{"b"}, Source: "option"}}
	if closed || !reflect.DeepEqual(answers, want) {
		t.Fatal(answers, closed)
	}
}

func TestCustomChoiceDraftAndTextEditing(t *testing.T) {
	d := newQuestionDialog(questionFixture())
	key := func(k tcell.Key) { d.key(tcell.NewEventKey(k, 0, 0), 8) }
	key(tcell.KeyEnd)
	key(tcell.KeyEnter)
	for _, r := range "ab界" {
		d.key(tcell.NewEventKey(tcell.KeyRune, r, 0), 8)
	}
	key(tcell.KeyBackspace2)
	key(tcell.KeyHome)
	d.key(tcell.NewEventKey(tcell.KeyRune, 'X', 0), 8)
	key(tcell.KeyDelete)
	key(tcell.KeyEnd)
	d.key(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModShift), 8)
	d.key(tcell.NewEventKey(tcell.KeyRune, 'Y', 0), 8)
	if got := string(d.answers[0].custom); got != "Xb\nY" {
		t.Fatal(got)
	}
	key(tcell.KeyEscape)
	key(tcell.KeyHome)
	d.key(tcell.NewEventKey(tcell.KeyRune, ' ', 0), 8)
	if d.answers[0].useCustom {
		t.Fatal("custom text mixed with option")
	}
	key(tcell.KeyEnd)
	key(tcell.KeyEnter)
	if string(d.answers[0].custom) != "Xb\nY" || !d.answers[0].useCustom {
		t.Fatal("custom draft lost")
	}
	if _, dismissed := d.key(tcell.NewEventKey(tcell.KeyEscape, 0, 0), 8); dismissed {
		t.Fatal("first Esc should leave text entry")
	}
	if _, dismissed := d.key(tcell.NewEventKey(tcell.KeyEscape, 0, 0), 8); !dismissed {
		t.Fatal("second Esc should dismiss without cancelling")
	}
}

func TestQuestionFreeTextOnlyAndNarrowViewport(t *testing.T) {
	d := newQuestionDialog(session.QuestionForm{ID: "form", Questions: []session.Question{{ID: "text", Prompt: "Describe the result."}}})
	if !d.answers[0].editing {
		t.Fatal("text-only form requires unnecessary choice")
	}
	d.key(tcell.NewEventKey(tcell.KeyRune, ' ', 0), 8)
	d.key(tcell.NewEventKey(tcell.KeyRight, 0, 0), 8)
	if values, _ := d.key(tcell.NewEventKey(tcell.KeyEnter, 0, 0), 8); values != nil {
		t.Fatal("blank text accepted")
	}
	for _, r := range strings.Repeat("界", 40) {
		d.key(tcell.NewEventKey(tcell.KeyRune, r, 0), 8)
	}
	d.reveal(12, 3)
	view := strings.Join(d.Window.Lines(12, 3), "\n")
	if !strings.Contains(view, "▏") {
		t.Fatal("text caret outside narrow viewport", view)
	}
	d.key(tcell.NewEventKey(tcell.KeyRight, 0, 0), 8)
	d.reveal(12, 3)
	if view := strings.Join(d.Window.Lines(12, 3), "\n"); !strings.Contains(view, "Submit") {
		t.Fatal(view)
	}
	if header := strings.Join(d.Window.HeaderLines(8, 1), "\n"); header != "[Submit]" {
		t.Fatal("active final tab hidden", header)
	}
}

func TestQuestionPasteDoesNotNavigateOrSubmit(t *testing.T) {
	d := newQuestionDialog(session.QuestionForm{ID: "form", Questions: []session.Question{{ID: "text", Prompt: "Text?"}}})
	d.pasting = true
	for _, event := range []*tcell.EventKey{tcell.NewEventKey(tcell.KeyRune, 'a', 0), tcell.NewEventKey(tcell.KeyTab, 0, 0), tcell.NewEventKey(tcell.KeyEnter, 0, 0), tcell.NewEventKey(tcell.KeyRune, 'b', 0), tcell.NewEventKey(tcell.KeyRight, 0, 0), tcell.NewEventKey(tcell.KeyEscape, 0, 0)} {
		if values, dismissed := d.key(event, 8); values != nil || dismissed || d.tab != 0 {
			t.Fatal("paste triggered navigation", values, dismissed, d.tab)
		}
	}
	if text := string(d.answers[0].custom); text != "a\t\nb" {
		t.Fatal(text)
	}
}

func TestQuestionContentPrecedesControlHints(t *testing.T) {
	d := newQuestionDialog(questionFixture())
	assertOrder := func(parts ...string) {
		t.Helper()
		end := 0
		for _, part := range parts {
			index := strings.Index(d.Window.Text[end:], part)
			if index < 0 {
				t.Fatalf("missing or misplaced %q:\n%s", part, d.Window.Text)
			}
			end += index + len(part)
		}
	}
	assertOrder("Which method?", "( ) First", "( ) Second (Recommended)", "Useful explanation", "Other · free-text input", "Up/Down choose", "←/→ tabs · Esc dismisses")
	if !d.Window.HideHint {
		t.Fatal("question controls should not precede content in the title bar")
	}

	d.key(tcell.NewEventKey(tcell.KeyEnd, 0, 0), 8)
	d.key(tcell.NewEventKey(tcell.KeyEnter, 0, 0), 8)
	for _, r := range "custom\nanswer" {
		d.insert(r)
	}
	d.update()
	assertOrder("Which method?", "Other · free-text input", "Text: custom\nanswer▏", "Enter finishes text", "←/→ tabs")
	if strings.Contains(d.Window.Text, "Up/Down choose") {
		t.Fatal("selection hints shown while editing text", d.Window.Text)
	}

	d.tab = len(d.answers)
	d.update()
	assertOrder("1. Which method?\n   custom\n   answer", "2. Any notes?\n   Unanswered", "3. Which check?\n   Unanswered", "> [ Submit answers ]", "Enter submits", "←/→ tabs · Esc dismisses")
	if d.rows[d.focusRow] != "> [ Submit answers ]" {
		t.Fatal("review focus moved from Submit to the footer", d.focusRow, d.rows)
	}
}

func TestQuestionTitleHasNoControlHints(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(100, 30)
	d := newQuestionDialog(questionFixture())
	drawWindow(s, &d.Window)
	left, top, width, _ := windowBounds(100, 30)
	var title strings.Builder
	for x := left; x < left+width; x++ {
		r, _, _, _ := s.GetContent(x, top)
		title.WriteRune(r)
	}
	if !strings.Contains(title.String(), "Questions") || strings.Contains(title.String(), "Esc") || strings.Contains(title.String(), "tabs") {
		t.Fatal(title.String())
	}
}
