package session

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/historytest"
	"ttc/internal/llm"
	"ttc/internal/tool"
)

func TestSubagentRequiresExplicitPersistence(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	for _, args := range []string{`{"prompt":"audit","label":"auditor"}`, `{"prompt":"audit","label":"auditor","persistent":null}`} {
		record := r.Tools.Invoke(context.Background(), tool.Execution{SessionID: r.Current(), Actor: "main", CallID: "not-admitted"}, "subagent", []byte(args))
		if !strings.Contains(string(record.Result), "persistent must be explicitly true or false") {
			t.Fatalf("missing persistence: %s", record.Result)
		}
		if len(r.ChildViews("main")) != 0 || len(r.Jobs.List("main", true)) != 0 {
			t.Fatal("invalid assignment created live state")
		}
	}
}

func TestSubagentEasySelection(t *testing.T) {
	for _, tc := range []struct {
		name             string
		configured, easy bool
		explicit         bool
	}{
		{"default", true, false, false}, {"false", true, false, true}, {"easy", true, true, true}, {"fallback", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			parent := r.CurrentSelection()
			easy := parent
			easy.Model.ID = "easy-model"
			easy.Variant = "easy-variant"
			if tc.configured {
				if err := r.RestoreEasyModel(easy); err != nil {
					t.Fatal(err)
				}
			}
			want := parent
			if tc.configured && tc.easy {
				want = easy
			}
			selections := make(chan llm.Selection, 4)
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				selections <- req.Selection
				return emit(llm.StreamEvent{Kind: "text", Text: "answer"})
			}}
			args := map[string]any{"prompt": "first", "label": "audit", "persistent": true}
			if tc.explicit {
				args["easy"] = tc.easy
			}
			encoded, _ := json.Marshal(args)
			seedRuntime(t, r, "Create a child")
			turn, err := r.Store.BeginTurn(r.Current(), "user", parent)
			if err != nil {
				t.Fatal(err)
			}
			admitted, _, err := r.admitMain(context.Background(), turn, parent)
			if err != nil {
				t.Fatal(err)
			}
			_, ids, err := r.Store.Assistant(r.Current(), turn, "main", admitted.RequestID, llm.Message{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: "create", Name: "subagent", Arguments: encoded}}})
			if err != nil {
				t.Fatal(err)
			}
			r.selection.Model.ID = "changed-parent"
			if err := r.RequestEasyModel(nil); err != nil {
				t.Fatal(err)
			}
			if !tc.configured {
				if err := r.RestoreEasyModel(easy); err != nil {
					t.Fatal(err)
				}
			}
			first := childInvocation(t, r, ids[0], string(encoded))
			if first["ok"] != true {
				t.Fatal(first)
			}
			if (first["easy"] == true) != tc.easy || r.ChildViews("main")[0].Easy != tc.easy {
				t.Fatalf("creation lost easy attribution: %v", first)
			}
			job, err := r.Jobs.View("main", first["job_id"].(string))
			if err != nil || job.Easy != tc.easy {
				t.Fatalf("job lost easy attribution: %+v, %v", job, err)
			}
			if got := receive(t, selections); !reflect.DeepEqual(got, want) {
				t.Fatalf("creation: got %+v want %+v", got, want)
			}
			var modelJSON string
			if err := historytest.DB(t, r.Store).QueryRow("SELECT model_json FROM model_requests WHERE actor_id=?", first["child_id"]).Scan(&modelJSON); err != nil {
				t.Fatal(err)
			}
			var recorded llm.Selection
			if err := json.Unmarshal([]byte(modelJSON), &recorded); err != nil || !reflect.DeepEqual(recorded, want) {
				t.Fatalf("recorded selection = %+v, want %+v: %v", recorded, want, err)
			}
			if err := r.Store.FinishTurn(turn, "completed"); err != nil {
				t.Fatal(err)
			}
			if err := r.RequestEasyModel(nil); err != nil {
				t.Fatal(err)
			}
			for _, flag := range []*bool{nil, new(bool), func() *bool { v := true; return &v }()} {
				args := map[string]any{"prompt": "follow", "child_id": first["child_id"], "persistent": true}
				if flag != nil {
					args["easy"] = *flag
				}
				follow, _ := json.Marshal(args)
				turn, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "follow", Name: "subagent", Arguments: follow}})
				result := childInvocation(t, r, ids[0], string(follow))
				if result["ok"] != true {
					t.Fatal(result)
				}
				if flag == nil {
					if result["warnings"] != nil {
						t.Fatal(result)
					}
				} else {
					warnings, ok := result["warnings"].([]any)
					if !ok || len(warnings) != 1 {
						t.Fatal(result)
					}
					warning := warnings[0].(map[string]any)
					if warning["code"] != "input_ignored" || warning["parameter"] != "easy" || warning["requested"] != *flag || warning["effective"] != tc.easy || warning["message"] == "" {
						t.Fatal(warning)
					}
				}
				if (result["easy"] == true) != tc.easy {
					t.Fatalf("follow-up lost frozen easy attribution: %v", result)
				}
				job, err := r.Jobs.View("main", result["job_id"].(string))
				if err != nil || job.Easy != tc.easy {
					t.Fatalf("follow-up job lost easy attribution: %+v, %v", job, err)
				}
				if got := receive(t, selections); !reflect.DeepEqual(got, want) {
					t.Fatalf("follow-up: got %+v want %+v", got, want)
				}
				if err := r.Store.FinishTurn(turn, "completed"); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestEasyModelSnapshotMatchesAdmissionAndSiblingCalls(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	seedRuntime(t, r, "Delegate")
	parent := r.CurrentSelection()
	easy := parent
	easy.Model.ID = "frozen-easy"
	if err := r.RestoreEasyModel(easy); err != nil {
		t.Fatal(err)
	}
	selections := make(chan llm.Selection, 2)
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		selections <- req.Selection
		return emit(llm.StreamEvent{Kind: "text", Text: "done"})
	}}
	turn, err := r.Store.BeginTurn(r.Current(), "user", parent)
	if err != nil {
		t.Fatal(err)
	}
	admitted, cm, err := r.admitMain(context.Background(), turn, parent)
	if err != nil {
		t.Fatal(err)
	}
	fields := runtimeFields(t, cm, "runtime_context")
	var announced easyModelContext
	if err := json.Unmarshal(fields["easy_model"], &announced); err != nil || !strings.Contains(announced.Model, "/frozen-easy/") {
		t.Fatalf("announcement: %s %v", cm.Content, err)
	}
	args := []byte(`{"prompt":"audit","label":"audit","persistent":false,"easy":true}`)
	_, ids, err := r.Store.Assistant(r.Current(), turn, "main", admitted.RequestID, llm.Message{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: "one", Name: "subagent", Arguments: args}, {ID: "two", Name: "subagent", Arguments: args}}})
	if err != nil {
		t.Fatal(err)
	}
	changed := easy
	changed.Model.ID = "later-easy"
	if err := r.RestoreEasyModel(changed); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if result := childInvocation(t, r, id, string(args)); result["ok"] != true {
			t.Fatal(result)
		}
		if got := receive(t, selections); !reflect.DeepEqual(got, easy) {
			t.Fatalf("sibling selected %+v, want %+v", got, easy)
		}
	}
}
