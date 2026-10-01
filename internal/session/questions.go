package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"scicode/internal/history"
	"scicode/internal/tool"
	"sort"
	"strings"
	"sync"
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

// QuestionForm is a snapshot of a pending runtime-only round, in question order.
type QuestionForm struct {
	ID, Actor string
	Questions []Question
	EntryID   int64 // Persisted tool intent, available for exact inspection.
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
type questionForm struct {
	view     QuestionForm
	reply    chan []Answer
	order    uint64
	answered bool
}
type questions struct {
	mu    sync.Mutex
	forms map[string]questionForm
	next  uint64
}

func (r *Runtime) addQuestionTool() {
	type args struct {
		Questions []Question `json:"questions"`
	}
	tool.Register(r.Tools, "question", "Ask 1–3 single-choice questions in a tabbed form. Each answer is exactly one option or free-text value. Optional recommended_option_id identifies one recommended choice; nothing submits automatically. Enter on an option advances to the next tab; only the final Submit tab sends the round. Plain mode accepts /answer FORM_ID JSON_ARRAY.", map[string]any{"questions": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"id": tool.Property("string"), "prompt": tool.Property("string"), "options": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"id": tool.Property("string"), "label": tool.Property("string"), "description": tool.Property("string")}, "required": []string{"id", "label"}, "additionalProperties": false}}, "recommended_option_id": tool.Property("string")}, "required": []string{"id", "prompt"}, "additionalProperties": false}}}, []string{"questions"}, func(a args) error {
		if len(a.Questions) < 1 || len(a.Questions) > 3 {
			return errors.New("require 1–3 questions")
		}
		ids := map[string]bool{}
		for _, q := range a.Questions {
			if q.ID == "" || q.Prompt == "" || ids[q.ID] {
				return errors.New("question IDs must be unique and prompts nonempty")
			}
			ids[q.ID] = true
			if q.Options != nil && (len(q.Options) < 2 || len(q.Options) > 5) {
				return errors.New("require 2–5 choices")
			}
			options := map[string]bool{}
			for _, o := range q.Options {
				if o.ID == "" || o.Label == "" || options[o.ID] {
					return errors.New("invalid or duplicate option")
				}
				options[o.ID] = true
			}
			if q.RecommendedOptionID != "" && !options[q.RecommendedOptionID] {
				return errors.New("recommended_option_id must identify an existing option")
			}
		}
		return nil
	}, func(ctx context.Context, x tool.Execution, a args) (any, error) {
		id := history.NewID("form")
		var entry int64
		if err := r.Store.DB.QueryRowContext(ctx, "SELECT id FROM entries WHERE kind='tool_call' AND json_extract(content_json,'$.call_id')=? ORDER BY id DESC LIMIT 1", x.CallID).Scan(&entry); err != nil {
			return nil, fmt.Errorf("question intent: %w", err)
		}
		view := QuestionForm{ID: id, Actor: x.Actor, Questions: a.Questions, EntryID: entry}
		form := questionForm{view: view, reply: make(chan []Answer, 1)}
		r.questions.mu.Lock()
		r.questions.next++
		form.order = r.questions.next
		r.questions.forms[id] = form
		r.questions.mu.Unlock()
		defer func() {
			r.questions.mu.Lock()
			delete(r.questions.forms, id)
			r.questions.mu.Unlock()
			r.emit(Event{Kind: "question_closed", Question: &QuestionForm{ID: id}})
		}()
		b, _ := json.MarshalIndent(a.Questions, "", "  ")
		eventView := cloneQuestionForm(view)
		r.emit(Event{Kind: "question", Question: &eventView, EntryID: entry, Text: fmt.Sprintf("Waiting for answer · %s\n%s\n/answer %s [{\"id\":\"QUESTION_ID\",\"values\":[\"text\"],\"source\":\"custom\"}]", id, b, id)})
		select {
		case answers := <-form.reply:
			return map[string]any{"answers": answers}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
}

// AnswerQuestion validates a whole form before completing the suspended call.
func (r *Runtime) AnswerQuestion(id string, answers []Answer) error {
	r.questions.mu.Lock()
	defer r.questions.mu.Unlock()
	form, ok := r.questions.forms[id]
	if !ok {
		return errors.New("question not found")
	}
	if form.answered {
		return errors.New("question already answered")
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
	select {
	case form.reply <- owned:
		form.answered = true
		r.questions.forms[id] = form
		return nil
	default:
		return errors.New("question already answered")
	}
}

// PendingQuestions returns unanswered forms in arrival order. Snapshots own their
// slices so a frontend cannot change the runtime's validation inputs.
func (r *Runtime) PendingQuestions() []QuestionForm {
	r.questions.mu.Lock()
	defer r.questions.mu.Unlock()
	var forms []questionForm
	for _, form := range r.questions.forms {
		if !form.answered {
			forms = append(forms, form)
		}
	}
	sort.Slice(forms, func(i, j int) bool { return forms[i].order < forms[j].order })
	views := make([]QuestionForm, 0, len(forms))
	for _, form := range forms {
		views = append(views, cloneQuestionForm(form.view))
	}
	return views
}

func cloneQuestionForm(view QuestionForm) QuestionForm {
	view.Questions = append([]Question(nil), view.Questions...)
	for i := range view.Questions {
		view.Questions[i].Options = append([]Option(nil), view.Questions[i].Options...)
	}
	return view
}
