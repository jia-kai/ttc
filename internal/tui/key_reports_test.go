package tui

import (
	"testing"
	"unicode/utf8"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/llm"
)

func TestComposerStringKeyReports(t *testing.T) {
	for _, text := range []string{"e\u0301", "👩🏽‍💻", "multiple runes"} {
		t.Run(text, func(t *testing.T) {
			var draft composer
			if !draft.key(tcell.NewEventKey(tcell.KeyRune, text, tcell.ModNone)) {
				t.Fatal("string key was not handled")
			}
			if draft.text != text || draft.cursor != utf8.RuneCountInString(text) {
				t.Fatalf("draft = %q at %d", draft.text, draft.cursor)
			}
		})
	}
}

func TestComposerLegacyAndAdvancedCtrlReports(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "advanced"}[advanced], func(t *testing.T) {
			key := func(letter string) *tcell.EventKey {
				if advanced {
					return tcell.NewEventKeyEx(tcell.KeyRune, letter, tcell.ModCtrl, true, 0, 1)
				}
				return tcell.NewEventKey(tcell.KeyRune, letter, tcell.ModCtrl)
			}
			var draft composer
			draft.insert("alpha beta")
			for _, letter := range []string{"w", "y", "a"} {
				if !draft.key(key(letter)) {
					t.Fatalf("Ctrl-%s was not handled", letter)
				}
			}
			if !draft.key(tcell.NewEventKey(tcell.KeyRune, "d", tcell.ModAlt)) || !draft.key(key("y")) {
				t.Fatal("word kill/yank was not handled")
			}
			for _, letter := range []string{"j", "k"} {
				if draft.key(key(letter)) || draft.text != "alpha beta" || draft.cursor != 5 {
					t.Fatalf("Ctrl-%s edited the draft: %q at %d", letter, draft.text, draft.cursor)
				}
			}
			if !draft.key(tcell.NewEventKey(tcell.KeyEnter, "", tcell.ModShift)) {
				t.Fatal("Shift-Enter was not handled")
			}
			if draft.text != "alpha\n beta" {
				t.Fatalf("draft = %q", draft.text)
			}
			before := draft.text
			if draft.key(key("q")) || draft.text != before {
				t.Fatal("unknown Ctrl report inserted text")
			}
		})
	}
}

func TestQuestionShiftTabReports(t *testing.T) {
	for _, event := range []*tcell.EventKey{
		tcell.NewEventKey(tcell.KeyTab, "", tcell.ModShift),
		tcell.NewEventKeyEx(tcell.KeyTab, "", tcell.ModShift, true, 0, 1),
	} {
		dialog := newQuestionDialog(questionFixture())
		dialog.tab = 2
		dialog.key(event, 8)
		if dialog.tab != 1 {
			t.Fatalf("Shift-Tab %v moved to tab %d", event, dialog.tab)
		}
	}
}

func TestFrontendIgnoresKeyReleaseReports(t *testing.T) {
	ui := newQuestionTestUI(t, &llm.Script{Responses: []llm.ScriptResponse{
		{Prefix: "user: kept!", Text: "Key reports verified."},
	}})
	for _, event := range []*tcell.EventKey{
		tcell.NewEventKeyEx(tcell.KeyCtrlC, "", tcell.ModCtrl, false, 0, 1),
		tcell.NewEventKeyEx(tcell.KeyRune, "discard", tcell.ModNone, false, 0, 1),
	} {
		ui.screen.EventQ() <- event
	}
	ui.typeText("kept")
	ui.screen.EventQ() <- tcell.NewEventKeyEx(tcell.KeyEnter, "", tcell.ModNone, false, 0, 1)
	ui.typeText("!")
	ui.wait(t, "> kept!")
	ui.key(tcell.KeyEnter)
	ui.wait(t, "Key reports verified.")
	ui.wait(t, "Turn complete")
}

func TestFrontendAdvancedControlAliases(t *testing.T) {
	ui := newQuestionTestUI(t, &llm.Script{Responses: []llm.ScriptResponse{
		{Prefix: "user: good\nline!", Text: "Control aliases verified."},
	}})
	ctrl := func(letter string) {
		ui.screen.EventQ() <- tcell.NewEventKeyEx(tcell.KeyRune, letter, tcell.ModCtrl, true, 0, 1)
	}
	ui.typeText("goodX")
	ctrl("h") // Backspace, not literal h.
	ctrl("j") // Scroll input, without inserting a newline.
	ctrl("k") // Scroll input, without killing the draft.
	ui.screen.EventQ() <- tcell.NewEventKeyEx(tcell.KeyEnter, "", tcell.ModShift, true, 0, 1)
	ui.typeText("line")
	ctrl("i") // Tab completion has no candidate here and must not insert i.
	ui.typeText("!")
	ctrl("m") // Enter submits the retained draft.
	ui.wait(t, "Control aliases verified.")
	ui.wait(t, "Turn complete")
	assertComposerSubmission(t, ui, "good\nline!")
}

func TestFrontendAdvancedCtrlShiftITabsBackward(t *testing.T) {
	ui := newQuestionTestUI(t, &llm.Script{Responses: questionScript()})
	ui.typeText("ask")
	ui.key(tcell.KeyEnter)
	ui.wait(t, "Choose a method?")
	ui.key(tcell.KeyRight)
	ui.wait(t, "Add notes?")
	ui.screen.EventQ() <- tcell.NewEventKeyEx(tcell.KeyRune, "i", tcell.ModCtrl|tcell.ModShift, true, 0, 1)
	ui.wait(t, "Choose a method?")
}
