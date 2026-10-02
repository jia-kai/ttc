package session

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"scicode/internal/provider"
	"scicode/internal/tool"
)

func TestSubagentRequiresExplicitPersistence(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	for _, args := range []string{
		`{"prompt":"audit","label":"auditor"}`,
		`{"prompt":"audit","label":"auditor","persistent":null}`,
	} {
		record := r.Tools.Invoke(context.Background(), tool.Execution{SessionID: r.Current(), Actor: "main", CallID: "not-admitted"}, "subagent", []byte(args))
		if !strings.Contains(string(record.Result), "persistent must be explicitly true or false") {
			t.Fatalf("missing persistence was not rejected before dispatch: %s", record.Result)
		}
		if len(r.ChildViews("main")) != 0 || len(r.Jobs.List("main", true)) != 0 {
			t.Fatal("invalid assignment created live state")
		}
	}
}

func TestSubagentVariantInheritanceAndIdleSelection(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	r.selection.Model.Variants = []string{"none", "low", "high"}
	r.selection.Variant = "high"
	frozen := r.CurrentSelection()
	selections := make(chan provider.Selection, 4)
	r.Provider = &childProvider{stream: func(ctx context.Context, request provider.Request, emit func(provider.StreamEvent) error) error {
		selections <- request.Selection
		return emit(provider.StreamEvent{Kind: "text", Text: "Audit answer"})
	}}
	firstArgs := `{"prompt":"first","label":"auditor","persistent":true}`
	turn, ids := batchIntents(t, r, "main", []provider.ToolCall{{ID: "create", Name: "subagent", Arguments: []byte(firstArgs)}})
	// Creation inherits the issuing request, even if the parent subsequently
	// switches its model/variant before this tool is executed.
	r.selection.Model.ID = "other-model"
	r.selection.Model.Variants = []string{"medium"}
	r.selection.Variant = "medium"
	first := childInvocation(t, r, ids[0], firstArgs)
	if first["ok"] != true {
		t.Fatal(first)
	}
	if got := receive(t, selections); !reflect.DeepEqual(got, frozen) {
		t.Fatalf("creation did not inherit the frozen request: %+v", got)
	}
	if err := r.Store.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	childID := first["child_id"].(string)
	for _, variant := range []string{"", "low", "none"} {
		args := map[string]any{"prompt": "follow up", "child_id": childID, "persistent": true}
		if variant != "" {
			args["variant"] = variant
		}
		encoded, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		turn, ids := batchIntents(t, r, "main", []provider.ToolCall{{ID: "follow-up", Name: "subagent", Arguments: encoded}})
		result := childInvocation(t, r, ids[0], string(encoded))
		if result["ok"] != true || result["child_id"] != childID {
			t.Fatal(result)
		}
		if variant != "" {
			frozen.Variant = variant
		}
		if got := receive(t, selections); !reflect.DeepEqual(got, frozen) {
			t.Fatalf("follow-up variant %q changed the child's model or lost its variant: %+v", variant, got)
		}
		if err := r.Store.FinishTurn(turn, "completed"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSubagentUnsupportedVariantDoesNotCreateChild(t *testing.T) {
	for _, variant := range []string{"", " ", "none", "unsupported"} {
		t.Run(variant, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			r.selection.Model.Variants = []string{"low", "high"}
			r.selection.Variant = "high"
			args, err := json.Marshal(map[string]any{"prompt": "audit", "label": "auditor", "persistent": true, "variant": variant})
			if err != nil {
				t.Fatal(err)
			}
			_, ids := batchIntents(t, r, "main", []provider.ToolCall{{ID: "invalid", Name: "subagent", Arguments: args}})
			result := childInvocation(t, r, ids[0], string(args))
			encoded, _ := json.Marshal(result)
			if result["ok"] == true || len(r.ChildViews("main")) != 0 || len(r.Jobs.List("main", true)) != 0 {
				t.Fatal("invalid variant created a child", result)
			}
			if strings.TrimSpace(variant) != "" && !strings.Contains(string(encoded), "supported variants: low, high") {
				t.Fatalf("missing supported-choice guidance: %s", encoded)
			}
			var childTurns int
			if err := r.Store.DB.QueryRow("SELECT count(*) FROM turns WHERE actor_id!='main'").Scan(&childTurns); err != nil || childTurns != 0 {
				t.Fatal("invalid variant admitted a child turn", childTurns, err)
			}
		})
	}
}

func TestSubagentExplicitCreationVariant(t *testing.T) {
	for _, variant := range []string{"low", "none"} {
		t.Run(variant, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			r.selection.Model.Variants = []string{"none", "low", "high"}
			r.selection.Variant = "high"
			selections := make(chan provider.Selection, 1)
			r.Provider = &childProvider{stream: func(ctx context.Context, request provider.Request, emit func(provider.StreamEvent) error) error {
				selections <- request.Selection
				return emit(provider.StreamEvent{Kind: "text", Text: "Answer"})
			}}
			args, _ := json.Marshal(map[string]any{"prompt": "audit", "label": "auditor", "persistent": false, "variant": variant})
			_, ids := batchIntents(t, r, "main", []provider.ToolCall{{ID: "create", Name: "subagent", Arguments: args}})
			result := childInvocation(t, r, ids[0], string(args))
			if result["ok"] != true {
				t.Fatal(result)
			}
			if got := receive(t, selections); got.Variant != variant || got.Model.ID != r.selection.Model.ID || got.Provider != r.selection.Provider {
				t.Fatalf("creation changed model/provider or ignored explicit variant: %+v", got)
			}
			var recorded string
			if err := r.Store.DB.QueryRow("SELECT json_extract(model_json,'$.variant') FROM model_requests WHERE actor_id=?", result["child_id"]).Scan(&recorded); err != nil || recorded != variant {
				t.Fatal("child request metadata lost selected variant", recorded, err)
			}
		})
	}
}

func TestSubagentInvalidVariantPreservesIdleChild(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	r.selection.Model.Variants = []string{"low", "high"}
	r.selection.Variant = "high"
	selections := make(chan provider.Selection, 2)
	r.Provider = &childProvider{stream: func(ctx context.Context, request provider.Request, emit func(provider.StreamEvent) error) error {
		selections <- request.Selection
		return emit(provider.StreamEvent{Kind: "text", Text: "Retained answer"})
	}}
	firstArgs := `{"prompt":"first","label":"auditor","persistent":true}`
	_, ids := batchIntents(t, r, "main", []provider.ToolCall{{ID: "create", Name: "subagent", Arguments: []byte(firstArgs)}})
	first := childInvocation(t, r, ids[0], firstArgs)
	if first["ok"] != true {
		t.Fatal(first)
	}
	receive(t, selections)
	childID := first["child_id"].(string)
	r.childStartMu.Lock()
	before := *r.children[childID]
	r.childStartMu.Unlock()
	for _, variant := range []string{"unsupported", "none", "", " "} {
		args, _ := json.Marshal(map[string]any{"prompt": "invalid follow-up", "child_id": childID, "persistent": false, "variant": variant})
		result := childInvocation(t, r, ids[0], string(args))
		if result["ok"] == true {
			t.Fatal("invalid follow-up accepted", result)
		}
		r.childStartMu.Lock()
		child := r.children[childID]
		unchanged := child != nil && reflect.DeepEqual(*child, before)
		r.childStartMu.Unlock()
		if !unchanged || len(r.Jobs.List("main", true)) != 1 || len(selections) != 0 {
			t.Fatal("invalid follow-up changed retained context or launched work")
		}
	}
	args, _ := json.Marshal(map[string]any{"prompt": "valid follow-up", "child_id": childID, "persistent": true, "variant": "low"})
	if result := childInvocation(t, r, ids[0], string(args)); result["ok"] != true {
		t.Fatal("invalid variants made the idle child unusable", result)
	}
	if got := receive(t, selections); got.Variant != "low" {
		t.Fatal(got)
	}
}
