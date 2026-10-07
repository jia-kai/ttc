package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	contextbuild "ttc/internal/context"
	"ttc/internal/history"
	"ttc/internal/prompts"
	"ttc/internal/tool"
	"unicode/utf8"
)

// Question presents one choice or free-text input. Pending forms are runtime-only.
type Question struct {
	ID                  string   `json:"id"`
	Prompt              string   `json:"prompt"`
	Options             []Option `json:"options,omitempty"`
	RecommendedOptionID string   `json:"recommended_option_id,omitempty"` // Optional existing option ID; never an automatic answer.
}

// MaxAnswerBytes bounds each custom answer's UTF-8 source bytes.
const MaxAnswerBytes = 16 << 10

// QuestionForm is a snapshot of a pending main-agent round, in question order.
type QuestionForm struct {
	ID        string
	Questions []Question
	EntryID   int64 // Persisted tool intent, available for exact inspection.
	Dismissed bool  // Hidden by the user; remains reopenable until the next human input.
}

// Option is an answer ID with a human-readable label.
type Option struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Answer preserves exactly one option ID or custom text, in question order.
type Answer struct {
	ID     string   `json:"id"`
	Values []string `json:"values"`
	Source string   `json:"source"`
}
type questionReply struct {
	Answers   []Answer `json:"answers,omitempty"`
	Dismissed bool     `json:"dismissed,omitempty"`
}

type questionForm struct {
	view     QuestionForm
	reply    chan questionReply
	resolved bool
}
type questions struct {
	mu      sync.Mutex
	pending *questionForm
}

func (r *Runtime) addQuestionTool() {
	type args struct {
		Questions []Question `json:"questions"`
	}
	tool.Register(r.Tools, "question", prompts.ToolDescription("question"), map[string]any{"questions": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"id": tool.Property("string"), "prompt": tool.Property("string"), "options": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"id": tool.Property("string"), "label": tool.Property("string"), "description": tool.Property("string")}, "required": []string{"id", "label"}, "additionalProperties": false}}, "recommended_option_id": tool.Property("string")}, "required": []string{"id", "prompt"}, "additionalProperties": false}}}, []string{"questions"}, func(a args) error {
		if len(a.Questions) < 1 || len(a.Questions) > 3 {
			return errors.New(prompts.QuestionCount)
		}
		ids := map[string]bool{}
		for _, q := range a.Questions {
			if q.ID == "" || q.Prompt == "" || ids[q.ID] {
				return errors.New(prompts.QuestionIdentityAndPrompt)
			}
			ids[q.ID] = true
			if q.Options != nil && (len(q.Options) < 2 || len(q.Options) > 5) {
				return errors.New(prompts.QuestionChoiceCount)
			}
			options := map[string]bool{}
			for _, o := range q.Options {
				if o.ID == "" || o.Label == "" || options[o.ID] {
					return errors.New(prompts.QuestionOptionIdentityAndLabel)
				}
				options[o.ID] = true
			}
			if q.RecommendedOptionID != "" && !options[q.RecommendedOptionID] {
				return errors.New(prompts.QuestionRecommendation)
			}
		}
		return nil
	}, func(ctx context.Context, x tool.Execution, a args) (any, error) {
		if x.Actor != "main" {
			return nil, errors.New(prompts.QuestionMainOnly)
		}
		id := history.NewID("form")
		var entry int64
		if err := r.Store.DB.QueryRowContext(ctx, "SELECT id FROM entries WHERE kind='tool_call' AND json_extract(content_json,'$.call_id')=? ORDER BY id DESC LIMIT 1", x.CallID).Scan(&entry); err != nil {
			return nil, fmt.Errorf(prompts.QuestionIntent, err)
		}
		view := QuestionForm{ID: id, Questions: a.Questions, EntryID: entry}
		form := &questionForm{view: view, reply: make(chan questionReply, 1)}
		r.questions.mu.Lock()
		if r.questions.pending != nil {
			r.questions.mu.Unlock()
			return nil, errors.New(prompts.QuestionAlreadyPending)
		}
		r.questions.pending = form
		r.questions.mu.Unlock()
		defer func() {
			r.questions.mu.Lock()
			if r.questions.pending == form {
				r.questions.pending = nil
			}
			r.questions.mu.Unlock()
			r.emit(Event{Kind: "question_closed", Question: &QuestionForm{ID: id}})
		}()
		b, _ := json.MarshalIndent(a.Questions, "", "  ")
		eventView := cloneQuestionForm(view)
		r.emit(Event{Kind: "question", Actor: x.Actor, Question: &eventView, EntryID: entry, Text: fmt.Sprintf("Waiting for answer · %s\n%s\n/answer %s [{\"id\":\"QUESTION_ID\",\"values\":[\"text\"],\"source\":\"custom\"}]", id, b, id)})
		select {
		case reply := <-form.reply:
			return reply, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
}

// AnswerQuestion validates a whole form before completing the suspended call.
func (r *Runtime) AnswerQuestion(id string, answers []Answer) error {
	r.questions.mu.Lock()
	defer r.questions.mu.Unlock()
	form := r.questions.pending
	if form == nil || form.view.ID != id {
		return errors.New("question not found")
	}
	if form.resolved {
		return errors.New("question already resolved")
	}
	if len(answers) != len(form.view.Questions) {
		return errors.New("answer count must match questions")
	}
	for i, a := range answers {
		q := form.view.Questions[i]
		if a.ID != q.ID || (a.Source != "custom" && a.Source != "option") {
			return errors.New("invalid answer identity/source")
		}
		if len(a.Values) != 1 {
			return errors.New("each question requires exactly one value")
		}
		if a.Source == "custom" && (strings.TrimSpace(a.Values[0]) == "" || len(a.Values[0]) > MaxAnswerBytes || !utf8.ValidString(a.Values[0])) {
			return fmt.Errorf("custom answer must contain 1–%d bytes of nonblank text", MaxAnswerBytes)
		}
		if a.Source == "option" {
			allowed := map[string]bool{}
			for _, o := range q.Options {
				allowed[o.ID] = true
			}
			if !allowed[a.Values[0]] {
				return errors.New("unknown option")
			}
		}
	}
	owned := append([]Answer(nil), answers...)
	for i := range owned {
		owned[i].Values = append([]string(nil), owned[i].Values...)
	}
	form.resolved = true
	form.reply <- questionReply{Answers: owned}
	return nil
}

// DismissQuestion hides a pending form without completing its tool call. It may
// be reopened or answered until RedirectDismissedQuestion accepts human input.
func (r *Runtime) DismissQuestion(id string) error {
	return r.setQuestionDismissed(id, true)
}

// ReopenQuestion restores a dismissed form to its waiting-for-answer state.
func (r *Runtime) ReopenQuestion(id string) error {
	return r.setQuestionDismissed(id, false)
}

func (r *Runtime) setQuestionDismissed(id string, dismissed bool) error {
	r.questions.mu.Lock()
	defer r.questions.mu.Unlock()
	form := r.questions.pending
	if form == nil || form.view.ID != id || form.resolved {
		return errors.New("pending question not found")
	}
	// UI-originated setters must not publish into the event queue that the UI
	// itself drains. Frontends reconcile their presentation from PendingQuestion.
	form.view.Dismissed = dismissed
	return nil
}

// RedirectDismissedQuestion queues human input at the next main model boundary
// and completes the dismissed form with {dismissed:true}. It returns
// true when it accepted the message; false leaves ordinary queuing to the caller.
// Local commands must not call this method. The publication gate makes input
// available before a resumed main request can consume the dismissal results.
func (r *Runtime) RedirectDismissedQuestion(input contextbuild.Input) (bool, error) {
	r.orderMu.Lock()
	defer r.orderMu.Unlock()
	r.questions.mu.Lock()
	defer r.questions.mu.Unlock()
	form := r.questions.pending
	if form == nil || !form.view.Dismissed || form.resolved {
		return false, nil
	}
	if err := validateSteeringInput(input); err != nil {
		return false, err
	}
	if err := r.checkContext(); err != nil {
		return false, err
	}
	r.steers = append(r.steers, input)
	form.resolved = true
	form.reply <- questionReply{Dismissed: true}
	return true, nil
}

// PendingQuestion returns nil when no unanswered form remains. The snapshot owns
// its slices so a frontend cannot change the runtime's validation inputs.
func (r *Runtime) PendingQuestion() *QuestionForm {
	r.questions.mu.Lock()
	defer r.questions.mu.Unlock()
	form := r.questions.pending
	if form == nil || form.resolved {
		return nil
	}
	view := cloneQuestionForm(form.view)
	return &view
}

func cloneQuestionForm(view QuestionForm) QuestionForm {
	view.Questions = append([]Question(nil), view.Questions...)
	for i := range view.Questions {
		view.Questions[i].Options = append([]Option(nil), view.Questions[i].Options...)
	}
	return view
}
