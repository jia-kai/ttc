package session

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ttc/internal/llm"
)

func assertChildCapabilities(t *testing.T, r *Runtime, request llm.Request) {
	t.Helper()
	var want []llm.ToolDefinition
	mainQuestion := false
	for _, definition := range r.Tools.Definitions() {
		mainQuestion = mainQuestion || definition.Name == "question"
		if definition.Name != "question" && definition.Name != "subagent" {
			want = append(want, definition)
		}
	}
	if !mainQuestion {
		t.Error("main lost the question tool")
	}
	if !reflect.DeepEqual(request.Tools, want) {
		t.Errorf("child capabilities differ from main minus question/subagent: %v", request.Tools)
	}
	for _, rule := range []string{
		"clarification rules override the generic",
		"tool or start a user dialog",
		"report material gaps or conflicting assumptions",
		"main agent in your final answer, and stop",
	} {
		if !strings.Contains(request.System, rule) {
			t.Errorf("child system lacks clarification rule %q", rule)
		}
	}
}

func TestSubagentCapabilitiesAcrossAssignmentsAndParentModelChange(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	r.selection.Model.Variants = []string{"none", "high"}
	original := r.CurrentSelection()
	requests := make(chan llm.Request, 4)
	r.Provider = &childProvider{stream: func(ctx context.Context, request llm.Request, emit func(llm.StreamEvent) error) error {
		requests <- request
		return emit(llm.StreamEvent{Kind: "text", Text: "Useful findings; main must resolve the missing dataset choice."})
	}}
	var childID string
	for assignment := range 4 {
		args := map[string]any{"prompt": "audit", "persistent": assignment < 2}
		if assignment == 0 || assignment == 3 {
			args["label"] = "audit"
		} else {
			args["child_id"] = childID
		}
		if assignment == 1 {
			// The retained child's model and capabilities stay frozen while the
			// parent changes model; a later new child receives the new model.
			r.selection.Model.ID = "changed-parent-model"
			args["variant"] = "high"
		}
		encoded, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		turn, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "assign", Name: "subagent", Arguments: encoded}})
		result := childInvocation(t, r, ids[0], string(encoded))
		if result["ok"] != true || result["status"] != "completed" {
			t.Fatal(result)
		}
		childID = result["child_id"].(string)
		request := receive(t, requests)
		assertChildCapabilities(t, r, request)
		wantModel := original.Model.ID
		if assignment == 3 {
			wantModel = "changed-parent-model"
		}
		if request.Selection.Model.ID != wantModel {
			t.Fatalf("assignment %d model = %q, want %q", assignment, request.Selection.Model.ID, wantModel)
		}
		if assignment == 1 && request.Selection.Variant != "high" {
			t.Fatal("follow-up did not change reasoning variant")
		}
		if err := r.Store.FinishTurn(turn, "completed"); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.ChildViews("main")) != 0 || len(r.Jobs.Live()) != 0 || r.PendingQuestion() != nil {
		t.Fatal("disposable assignment retained live state")
	}
}

func TestSubagentQuestionCallRejectedAndGapReportedToMain(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("background=%t", background), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			var openedQuestion atomic.Bool
			r.Emit = func(event Event) {
				if event.Kind == "question" {
					openedQuestion.Store(true)
				}
			}
			const answer = "Audited the available inputs. The dataset choice materially affects implementation; main must clarify it. Stopping here."
			var childRequests atomic.Int32
			var mainRequests atomic.Int32
			var surfaced atomic.Bool
			r.Provider = &childProvider{stream: func(ctx context.Context, request llm.Request, emit func(llm.StreamEvent) error) error {
				if strings.HasPrefix(request.ConversationID, "main/child") {
					assertChildCapabilities(t, r, request)
					if childRequests.Add(1) == 1 {
						// A provider may emit a tool that was never advertised. It
						// must get the ordinary missing-tool error, not a dialog or
						// an implicit conversion into a message to the main agent.
						call := llm.ToolCall{ID: "forbidden-question", Name: "question", Arguments: []byte(`{"questions":[{"id":"dataset","prompt":"Which dataset?"}]}`)}
						return emit(llm.StreamEvent{Kind: "call", Call: &call})
					}
					var last llm.Message
					for _, message := range request.Messages {
						if message.Role == "tool" && message.CallID == "forbidden-question" {
							last = message
						}
					}
					var failure struct {
						OK    bool `json:"ok"`
						Error struct {
							Code    string `json:"code"`
							Message string `json:"message"`
						} `json:"error"`
					}
					if last.Role != "tool" || last.CallID != "forbidden-question" {
						return fmt.Errorf("missing ordinary tool failure: %+v", last)
					}
					if err := json.Unmarshal([]byte(last.Content), &failure); err != nil {
						return err
					}
					if failure.OK || failure.Error.Code != "unknown_tool" || !strings.Contains(failure.Error.Message, "question") {
						return fmt.Errorf("unclear missing-tool error: %s", last.Content)
					}
					if r.PendingQuestion() != nil {
						return fmt.Errorf("child created a pending question")
					}
					return emit(llm.StreamEvent{Kind: "text", Text: answer})
				}
				mainQuestion := false
				for _, definition := range request.Tools {
					mainQuestion = mainQuestion || definition.Name == "question"
				}
				if !mainQuestion {
					return fmt.Errorf("main request lost question capability")
				}
				if mainRequests.Add(1) == 1 {
					args := fmt.Sprintf(`{"prompt":"Audit inputs; report missing information and stop","label":"input audit","persistent":false,"background":%t}`, background)
					call := llm.ToolCall{ID: "assign", Name: "subagent", Arguments: []byte(args)}
					return emit(llm.StreamEvent{Kind: "call", Call: &call})
				}
				for _, message := range request.Messages {
					if (message.Role == "tool" || message.Runtime) && strings.Contains(message.Content, answer) {
						surfaced.Store(true)
					}
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Main will clarify the reported gap."})
			}}
			done := make(chan error, 1)
			go func() {
				message := llm.Message{Role: "user", Content: "Audit the inputs"}
				done <- r.Run(&message)
			}()
			if err := receive(t, done); err != nil {
				t.Fatal(err)
			}
			jobs := r.Jobs.List("main", true)
			if len(jobs) != 1 {
				t.Fatal(jobs)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			job, err := r.Jobs.Wait(ctx, "main", jobs[0].ID, nil)
			if err != nil || job.Status != "completed" || !strings.Contains(job.Stdout, answer) {
				t.Fatal("child failed to exit without user input", job, err)
			}
			if background {
				// Admit the completion notification whether it arrived during
				// the first main turn or after that turn finished.
				message := llm.Message{Role: "user", Content: "Review the child findings"}
				if err := r.Run(&message); err != nil {
					t.Fatal(err)
				}
			}
			if childRequests.Load() != 2 || !surfaced.Load() {
				t.Fatal("missing correction or answer in main context", childRequests.Load(), surfaced.Load())
			}
			if openedQuestion.Load() || r.PendingQuestion() != nil || len(r.ChildViews("main")) != 0 || len(r.Jobs.Live()) != 0 {
				t.Fatal("rejected child call left a form or blocked worker")
			}
			var result string
			if err := r.Store.DB.QueryRow("SELECT result_json FROM tool_calls WHERE name='question'").Scan(&result); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(result, "unknown_tool") {
				t.Fatal("missing persisted rejection", result)
			}
		})
	}
}
