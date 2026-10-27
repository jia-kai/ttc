package tui

import (
	"encoding/json"
	"fmt"
	"time"
	"ttc/internal/session"
)

func (c *frontendController) syncQuestionState(form *session.QuestionForm) {
	c.activity.syncQuestion(form, time.Now())
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
	if c.modal.question != nil && (form == nil || form.ID != c.modal.question.form.ID || form.Dismissed) {
		c.modal.clear()
		c.esc, c.viewChord = false, false
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
	c.modal.clear()
	c.modal.generation = c.frontend.Runtime.Generation()
	c.modal.question = c.retainedQuestion
	c.modal.window = &c.retainedQuestion.Window
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
	if err := c.frontend.Runtime.ReopenQuestion(form.ID); err != nil {
		c.add("Reopen failed: "+err.Error(), 0)
		return
	}
	form = c.frontend.Runtime.PendingQuestion()
	c.syncQuestionState(form)
	if form != nil {
		c.showQuestion(form)
	}
}
