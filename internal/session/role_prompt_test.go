package session

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"ttc/internal/historytest"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// Exercise the receiving boundary rather than only checking prompt constants:
// capability filtering, admission and the persisted inspector must all agree.
func TestReceivingRolePromptsAndPersistedSystems(t *testing.T) {
	for _, role := range []string{"main", "child", "aside"} {
		t.Run(role, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			requests := make(chan llm.Request, 1)
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				requests <- req
				return emit(llm.StreamEvent{Kind: "text", Text: "Useful evidence; the input choice remains unresolved."})
			}}
			switch role {
			case "main":
				message := llm.Message{Type: llm.TypeUserInput, Content: "Review the inputs"}
				if err := r.Run(&message); err != nil {
					t.Fatal(err)
				}
			case "child":
				args := `{"prompt":"Review the inputs","label":"input audit","persistent":false}`
				turn, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "assign", Name: "subagent", Arguments: []byte(args)}})
				if result := childInvocation(t, r, ids[0], args); result["ok"] != true {
					t.Fatal(result)
				}
				if err := r.Store.FinishTurn(turn, "completed"); err != nil {
					t.Fatal(err)
				}
			case "aside":
				seedRuntime(t, r, "Review the inputs")
				id, err := r.StartBTW("What information is missing?")
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				job, err := r.Jobs.Wait(ctx, "main", id, nil)
				if err != nil || job.Status != "completed" {
					t.Fatal(job, err)
				}
			}
			req := receive(t, requests)
			assertReceivingRolePrompt(t, r, role, req)
		})
	}
}

func assertReceivingRolePrompt(t *testing.T, r *Runtime, role string, req llm.Request) {
	t.Helper()
	assertPersistedReceivedSystem(t, r, req)
	var names []string
	var received strings.Builder
	received.WriteString(req.System)
	for _, definition := range req.Tools {
		names = append(names, definition.Name)
		received.WriteString("\n" + definition.Description)
	}
	sort.Strings(names)
	switch role {
	case "main":
		if req.System != prompts.System+"\n"+prompts.Coding+"\n"+prompts.Main || !reflect.DeepEqual(req.Tools, r.Tools.Definitions()) {
			t.Error("main lost role guidance or capabilities")
		}
		want := []string{"await", "child_message", "conversation_export", "edit", "glob", "grep", "image_show", "job_list", "job_read", "job_stop", "lsp_query", "patch", "question", "read", "shell", "skill", "subagent", "tmux_watch_run", "wakeup_cancel", "wakeup_list", "wakeup_schedule", "wakeup_update", "watch", "web_fetch", "web_search", "write"}
		if !reflect.DeepEqual(names, want) {
			t.Errorf("main advertised capabilities = %v, want %v", names, want)
		}
		for _, guidance := range []string{"use question", "Use subagent"} {
			if !strings.Contains(req.System, guidance) {
				t.Errorf("main missing %q guidance", guidance)
			}
		}
	case "child":
		assertChildCapabilities(t, r, req)
	case "aside":
		if req.System != prompts.System {
			t.Error("aside received coding or main role instructions")
		}
		want := []string{"conversation_export", "glob", "grep", "job_list", "job_read", "lsp_query", "read", "skill", "wakeup_list", "web_fetch", "web_search"}
		if !reflect.DeepEqual(names, want) {
			t.Errorf("aside advertised capabilities = %v, want %v", names, want)
		}
		foundScope := false
		for _, message := range req.Messages {
			foundScope = foundScope || message.Role() == "developer" && message.Type.IsRuntime() && message.Content == prompts.Btw
		}
		if !foundScope {
			t.Error("aside missing separate-question runtime scope")
		}
		for _, guidance := range []string{"Use write", "Use edit", "Use patch", "Use foreground shell", "background shell jobs", "persistent=false", "job_stop(child_id="} {
			if strings.Contains(received.String(), guidance) {
				t.Errorf("aside received irrelevant call guidance %q", guidance)
			}
		}
	}
	if role != "main" {
		for _, guidance := range []string{"use question", "Use subagent"} {
			if strings.Contains(received.String(), guidance) {
				t.Errorf("%s received main-only call guidance %q", role, guidance)
			}
		}
	}
}

func assertPersistedReceivedSystem(t *testing.T, r *Runtime, req llm.Request) {
	t.Helper()
	var entryID int64
	actor := req.ConversationID
	if actor == r.Current() {
		actor = "main"
	}
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT id FROM entries WHERE actor_id=? AND json_extract(content_json,'$.type')='system_prompt' ORDER BY id DESC LIMIT 1`, actor).Scan(&entryID); err != nil {
		t.Fatal(err)
	}
	entry, err := r.Store.Entry(entryID)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := r.Store.ReadInspection(context.Background(), entry.ID)
	if err != nil || saved.Text != req.System {
		t.Fatal("persisted inspector differs from received system", err)
	}
}

func TestAsideAdmissionBudgetsSharedOnlySystem(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Main task")
	actor := "main/btw_budget"
	turn, err := r.Store.BeginChildTurn(r.Current(), actor, r.selection)
	if err != nil {
		t.Fatal(err)
	}
	messages := []llm.Message{{Type: llm.TypeRuntimeContext, Content: btwInstruction}, {Type: llm.TypeUserInput, Content: "Separate question"}}
	cm, _, err := r.runtimeContext(context.Background(), actor, r.selection, contextCursor{})
	if err != nil {
		t.Fatal(err)
	}
	input := append(append([]llm.Message(nil), messages...), *cm)
	usage := estimateUsage(r.selection, asideSystemTemplate, r.Tools.Definitions(), input)
	// Keep all schemas identical here to isolate the system contribution. A
	// main-sized prompt must fail while the actual aside prompt just fits.
	r.selection.Model.Budget.ContextLimit = usage.Input + usage.Reserved + 1
	if !contextbuild.Fits(r.selection, asideSystemTemplate, r.Tools.Definitions(), input, false) || contextbuild.Fits(r.selection, systemTemplate, r.Tools.Definitions(), input, false) {
		t.Fatal("fixture does not distinguish shared-only from main prompt budgeting")
	}
	if _, _, err := r.admitChild(context.Background(), childTask{actor: actor, turn: turn, selection: r.selection, tools: r.Tools, aside: true, estimator: &contextbuild.Estimator{}}, messages, contextCursor{}); err != nil {
		t.Fatal("aside admission budgeted irrelevant role instructions", err)
	}
}
