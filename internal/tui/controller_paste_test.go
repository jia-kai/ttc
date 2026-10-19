package tui

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/session"
)

func pasteTestController() *frontendController {
	return &frontendController{
		frontend: &Frontend{Runtime: &session.Runtime{}, Output: io.Discard},
		ctx:      context.Background(), draft: newComposer(""),
		completer: &pathCompleter{}, ticker: &time.Ticker{},
		view: newTranscript(), names: map[string]string{}, sidebar: newSidebar(),
	}
}

func pasteRune(text string) *tcell.EventKey { return tcell.NewEventKey(tcell.KeyRune, text, 0) }

func TestPasteBatchBulkEditingAndLiteralKeys(t *testing.T) {
	c := pasteTestController()
	c.draft.set("前後")
	c.draft.cursor = 1
	c.handlePaste(tcell.NewEventPaste(true))
	c.batchingEvents = true
	for _, ev := range []*tcell.EventKey{
		pasteRune("A"), tcell.NewEventKey(tcell.KeyEnter, "", tcell.ModAlt),
		tcell.NewEventKey(tcell.KeyTab, "", 0), pasteRune("界"),
		tcell.NewEventKey(tcell.KeyEscape, "", 0), tcell.NewEventKey(tcell.KeyCtrlR, "", 0),
		tcell.NewEventKeyEx(tcell.KeyRune, "released", 0, false, 0, 0),
	} {
		if next := c.handleKey(ev); next.ready {
			t.Fatal("paste submitted a prompt")
		}
	}
	if c.draft.text != "前後" || c.paste.text.String() != "A\n\t界" {
		t.Fatalf("per-event edit instead of collection: %q, %q", c.draft.text, c.paste.text.String())
	}
	c.flushPaste()
	if c.draft.text != "前A\n\t界後" || c.draft.cursor != 5 || c.modal.prompts != nil {
		t.Fatalf("bulk insertion = %q, cursor %d", c.draft.text, c.draft.cursor)
	}
	if !c.paste.active {
		t.Fatal("partial feedback ended paste early")
	}
	c.handlePaste(tcell.NewEventPaste(false))
	if c.paste.active || c.draft.pasting {
		t.Fatal("paste end did not release destination")
	}
}

func TestPasteBatchCtrlCClearsPartialDraft(t *testing.T) {
	c := pasteTestController()
	c.handlePaste(tcell.NewEventPaste(true))
	c.batchingEvents = true
	c.handleKey(pasteRune("partial"))
	if next := c.handleKey(tcell.NewEventKey(tcell.KeyCtrlC, "", 0)); next.ready || c.draft.text != "" {
		t.Fatalf("Ctrl+C did not clear pending partial draft: %+v, %q", next, c.draft.text)
	}
	c.handleKey(pasteRune("after"))
	c.flushPaste()
	if c.draft.text != "after" {
		t.Fatalf("text after Ctrl+C = %q", c.draft.text)
	}
	if next := c.handleKey(tcell.NewEventKey(tcell.KeyCtrlC, "", 0)); next.ready {
		t.Fatal("Ctrl+C quit instead of clearing nonempty draft")
	}
	if next := c.handleKey(tcell.NewEventKey(tcell.KeyCtrlC, "", 0)); !next.ready || next.text != "/quit" {
		t.Fatalf("Ctrl+C on empty draft = %+v", next)
	}
}

func pasteQuestionFixture() *questionDialog {
	return newQuestionDialog(session.QuestionForm{ID: "paste", Questions: []session.Question{{ID: "answer", Prompt: "Answer?"}}})
}

func TestPasteBatchQuestionLimitsMatchDecodedEvents(t *testing.T) {
	for _, final := range []string{"z", "界", "\xff"} {
		t.Run(fmt.Sprintf("%x", final), func(t *testing.T) {
			c := pasteTestController()
			d := pasteQuestionFixture()
			initial := strings.Repeat("x", session.MaxAnswerBytes-2)
			d.answers[0].custom.set(initial)
			d.answers[0].custom.cursor = 3
			c.modal.question, c.modal.window = d, &d.Window
			c.handlePaste(tcell.NewEventPaste(true))
			c.batchingEvents = true
			for _, text := range []string{strings.Repeat("oversized", session.MaxAnswerBytes), "a", "界", final} {
				if next := c.handleKey(pasteRune(text)); next.ready {
					t.Fatal("question paste submitted")
				}
			}
			c.flushPaste()
			inserted, wantError := "a", ""
			switch final {
			case "z":
				inserted += final
			case "界":
				wantError = fmt.Sprintf("Text is limited to %d bytes.", session.MaxAnswerBytes)
			default:
				wantError = "Text must be valid UTF-8."
			}
			got, wantText := d.answers[0].custom, initial[:3]+inserted+initial[3:]
			if got.text != wantText || got.cursor != 3+len(inserted) || d.errorText != wantError || d.tab != 0 {
				t.Fatalf("question paste mismatch: bytes %d/%d, cursor %d, error %q/%q, tab %d", len(got.text), len(wantText), got.cursor, d.errorText, wantError, d.tab)
			}
			if c.draft.text != "" || !c.paste.active || c.paste.question != d {
				t.Fatal("question paste escaped its destination")
			}
		})
	}
}

func TestPasteBatchQuestionRejectionPreservesEntireDraft(t *testing.T) {
	c := pasteTestController()
	d := pasteQuestionFixture()
	d.answers[0].custom.set(strings.Repeat("x", session.MaxAnswerBytes))
	d.answers[0].custom.cursor = 10
	d.answers[0].custom.killed = "界"
	before := d.answers[0].custom
	c.modal.question, c.modal.window = d, &d.Window
	c.handleScreenEvent(tcell.NewEventPaste(true))
	if next := c.handleScreenEvent(tcell.NewEventKey(tcell.KeyEnter, "", 0)); next.ready || d.tab != 0 {
		t.Fatal("rejected paste navigated or submitted", next, d.tab)
	}
	if !reflect.DeepEqual(before, d.answers[0].custom) || !strings.Contains(d.errorText, "limited") {
		t.Fatal("rejected paste changed cursor, kill buffer, or draft", d.errorText)
	}
	c.handleScreenEvent(tcell.NewEventPaste(false))
	d.key(tcell.NewEventKey(tcell.KeyBackspace, "", 0), 8)
	d.key(tcell.NewEventKey(tcell.KeyEnter, "", tcell.ModShift), 8)
	if len(d.answers[0].custom.text) != session.MaxAnswerBytes || d.errorText != "" {
		t.Fatal("could not resume editing after rejected paste", d.errorText)
	}
}

func TestPasteBatchSearchLimitMatchesDecodedEvents(t *testing.T) {
	c := pasteTestController()
	m := newPromptSearch([]string{"first", "second"})
	initial := strings.Repeat("界", 254)
	m.query.set(initial)
	m.query.cursor = 3
	c.modal.prompts, c.modal.window = m, &m.Window
	c.handlePaste(tcell.NewEventPaste(true))
	c.batchingEvents = true
	for _, ev := range []*tcell.EventKey{pasteRune("ab界xyz"), pasteRune("more"), tcell.NewEventKey(tcell.KeyEnter, "", 0)} {
		if next := c.handleKey(ev); next.ready {
			t.Fatal("search paste submitted")
		}
	}
	c.flushPaste()
	want := strings.Repeat("界", 3) + "ab界xyz" + strings.Repeat("界", 247)
	if m.query.text != want || m.query.cursor != 256 || utf8.RuneCountInString(m.query.text) != 256 {
		t.Fatalf("search truncation differs: cursor %d, runes %d", m.query.cursor, utf8.RuneCountInString(m.query.text))
	}
	if c.draft.text != "" || c.modal.prompts != m {
		t.Fatal("search paste recalled or submitted a prompt")
	}
}

func TestPasteBatchDestinationOwnership(t *testing.T) {
	for _, destination := range []string{"question", "search", "blocked", "composer"} {
		t.Run(destination, func(t *testing.T) {
			c := pasteTestController()
			original := pasteQuestionFixture()
			switch destination {
			case "question":
				c.modal.question, c.modal.window = original, &original.Window
			case "search":
				c.modal.prompts = newPromptSearch(nil)
			case "blocked":
				c.modal.commands = newCommandMenu()
			}
			c.handlePaste(tcell.NewEventPaste(true))
			replacement := pasteQuestionFixture()
			c.modal.clear()
			c.modal.question, c.modal.window = replacement, &replacement.Window
			c.handleKey(pasteRune("remaining"))
			c.handleKey(tcell.NewEventKey(tcell.KeyEnter, "", 0))
			if original.answers[0].custom.text != "" || replacement.answers[0].custom.text != "" || replacement.tab != 0 {
				t.Fatal("paste redirected to a new question")
			}
			want := ""
			if destination == "composer" {
				want = "remaining\n"
			}
			if c.draft.text != want {
				t.Fatalf("composer text = %q, want %q", c.draft.text, want)
			}
			c.handlePaste(tcell.NewEventPaste(false))
		})
	}
}

func TestPasteBatchReceiveBoundedPartialFeedbackAndRuntime(t *testing.T) {
	c := pasteTestController()
	keys := make(chan tcell.Event, 3*frontendBatchEvents)
	c.keys = keys
	c.handlePaste(tcell.NewEventPaste(true))
	for i := 0; i < cap(keys); i++ {
		keys <- pasteRune("x")
	}
	events := make(chan session.Event, 1)
	events <- session.Event{Kind: "status", Text: "runtime remains responsive"}
	c.frontend.Events = events
	before := len(keys)
	if next, stop, err := c.receive(); err != nil || stop || next.ready {
		t.Fatalf("receive = %+v, %v, %v", next, stop, err)
	}
	consumed := before - len(keys)
	if consumed > frontendBatchEvents || consumed < 2 || len(keys) == 0 {
		t.Fatalf("unbounded or unbatched receive: consumed %d, remaining %d", consumed, len(keys))
	}
	if c.draft.text == "" || !c.paste.active || c.paste.text.Len() != 0 || c.batchingEvents {
		t.Fatal("receive did not flush partial paste before its end marker")
	}
	if len(events) != 0 || len(c.view.lines) != 1 || c.view.lines[0].text != "runtime remains responsive" {
		t.Fatal("runtime event starved behind paste")
	}
}

func TestPasteBatchQuestionPartialBeforeEnd(t *testing.T) {
	c := pasteTestController()
	d := pasteQuestionFixture()
	c.modal.question, c.modal.window = d, &d.Window
	keys := make(chan tcell.Event, 3)
	c.keys = keys
	keys <- tcell.NewEventPaste(true)
	keys <- pasteRune("partial")
	keys <- tcell.NewEventKey(tcell.KeyEnter, "", 0)
	if next, stop, err := c.receive(); err != nil || stop || next.ready {
		t.Fatalf("receive = %+v, %v, %v", next, stop, err)
	}
	if d.answers[0].custom.text != "partial\n" || d.tab != 0 || !c.paste.active {
		t.Fatalf("partial question = %q, tab %d, pasting %v", d.answers[0].custom.text, d.tab, c.paste.active)
	}
}

func TestPasteBatchPreservesOrdinaryKeyPrepareBoundaries(t *testing.T) {
	c := pasteTestController()
	keys := make(chan tcell.Event, 5)
	c.keys = keys
	keys <- pasteRune("/")
	keys <- tcell.NewEventKey(tcell.KeyTab, "", 0)
	if next, stop, err := c.receive(); err != nil || stop || next.ready {
		t.Fatalf("receive = %+v, %v, %v", next, stop, err)
	}
	if len(keys) != 1 || c.draft.text != "/" {
		t.Fatal("ordinary keys coalesced across completion prepare boundary")
	}
	<-keys
	keys <- tcell.NewEventPaste(true)
	keys <- pasteRune("literal")
	keys <- tcell.NewEventPaste(false)
	keys <- tcell.NewEventKey(tcell.KeyEnter, "", 0)
	if next, stop, err := c.receive(); err != nil || stop || next.ready {
		t.Fatalf("receive = %+v, %v, %v", next, stop, err)
	}
	if len(keys) != 1 || c.draft.text != "/literal" || c.paste.active {
		t.Fatal("paste-end coalesced with following ordinary Enter")
	}
}

func BenchmarkPastePerDecodedEvent(b *testing.B) {
	for _, size := range []int{1 << 10, 16 << 10} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			key := pasteRune("x")
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				c := newComposer("")
				for n := 0; n < size; n++ {
					c.insert(decodedPasteText(key))
				}
				if len(c.text) != size {
					b.Fatal("lost paste text")
				}
			}
		})
	}
}

func BenchmarkPasteBatch(b *testing.B) {
	for _, size := range []int{1 << 10, 16 << 10, 64 << 10} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			key := pasteRune("x")
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				c := pasteTestController()
				c.handlePaste(tcell.NewEventPaste(true))
				c.batchingEvents = true
				for n := 0; n < size; n++ {
					c.handleKey(key)
					if (n+1)%frontendBatchEvents == 0 {
						c.flushPaste()
					}
				}
				c.handlePaste(tcell.NewEventPaste(false))
				if len(c.draft.text) != size {
					b.Fatal("lost paste text")
				}
			}
		})
	}
}
