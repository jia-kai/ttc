package tui

import (
	"encoding/json"
	"fmt"
	"time"
	"ttc/internal/session"
)

// focusedQuestion returns the inline form outside copy mode and modal focus.
func (c *frontendController) focusedQuestion() *questionDialog {
	if c.fullscreen || !c.modal.empty() {
		return nil
	}
	return c.question
}

// revealComposer gives an accepted palette/history result the bottom input.
// Dismissal stays asynchronous; the form ID fences publication already queued
// before that control commits, and only explicit reopening releases the fence.
func (c *frontendController) revealComposer() bool {
	if form := c.frontend.Runtime.PendingQuestion(); form != nil {
		// Submit even if already dismissed: an older reopen may still be queued.
		if !c.control(session.Request{Kind: session.RequestDismissQuestion, ID: form.ID}) {
			return false
		}
		c.composerQuestionID = form.ID
	}
	c.question = nil
	c.deferredQuestionID, c.questionReopenRequest = "", 0
	c.esc, c.viewChord = false, false
	c.sidebar.overlay = false
	c.setFullscreen(false)
	return true
}

func (c *frontendController) syncQuestionState(form *session.QuestionForm) {
	c.activity.syncQuestion(form, time.Now())
	if form == nil || form.ID != c.composerQuestionID {
		c.composerQuestionID = ""
	}
	if c.questionRow >= len(c.view.lines) || c.questionRow >= 0 && c.view.lines[c.questionRow].questionID == "" {
		c.questionRow = -1
	}
	if c.questionRow >= 0 {
		item := c.view.lines[c.questionRow]
		label := "Question closed · " + item.questionID
		live := form != nil && form.ID == item.questionID
		if live {
			label = questionStatus(*form)
		}
		if item.text != label {
			item.text = label
			c.view.replace(c.questionRow, item)
			if c.frontend.Plain {
				fmt.Fprintln(c.frontend.Output, label)
			}
		}
		if !live {
			c.questionRow = -1
		}
	}
	if form != nil && c.questionRow < 0 {
		c.view.append(c.attribute(line{text: questionStatus(*form), id: form.EntryID, action: actionQuestion, inspectID: form.EntryID, system: true, questionID: form.ID}, "main"))
		c.questionRow = len(c.view.lines) - 1
	}
	if c.retainedQuestion != nil && (form == nil || form.ID != c.retainedQuestion.form.ID) {
		c.retainedQuestion = nil
	}
	if c.question != nil && (form == nil || form.ID != c.question.form.ID || form.Dismissed) {
		focused := c.focusedQuestion() != nil
		c.question = nil
		if focused {
			c.esc, c.viewChord = false, false
		}
	}
	if form == nil || form.ID != c.deferredQuestionID || form.Dismissed {
		c.deferredQuestionID = ""
	}
}

func (c *frontendController) showQuestion(form *session.QuestionForm) {
	c.esc, c.viewChord = false, false
	c.deferredQuestionID = ""
	if c.frontend.Plain {
		data, _ := json.MarshalIndent(form.Questions, "", "  ")
		c.add("Pending question · "+form.ID+"\n"+string(data), form.EntryID)
		return
	}
	// Answering uses the live conversation, not the frozen copy-mode snapshot.
	c.setFullscreen(false)
	c.sidebar.overlay = false
	if c.retainedQuestion == nil || c.retainedQuestion.form.ID != form.ID {
		c.retainedQuestion = newQuestionDialog(*form)
	}
	c.retainedQuestion.form = *form
	c.question = c.retainedQuestion
}

func (c *frontendController) openQuestions(id string) {
	c.esc, c.viewChord = false, false
	form := c.frontend.Runtime.PendingQuestion()
	if form == nil {
		c.add("No pending question", 0)
		return
	}
	if id != "" && form.ID != id {
		c.add("Error: pending question not found · "+id, 0)
		return
	}
	if !c.frontend.Plain {
		// Reveal explicit user intent now; the asynchronous acknowledgment must
		// still defer to any newer interaction that takes focus before it arrives.
		c.modal.clear()
		c.setFullscreen(false)
		c.sidebar.overlay = false
	}
	if c.control(session.Request{Kind: session.RequestReopenQuestion, ID: form.ID}) {
		c.composerQuestionID = ""
	}
}
