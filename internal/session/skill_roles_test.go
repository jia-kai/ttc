package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"ttc/internal/llm"
	"ttc/internal/skills"
	"ttc/internal/tool"
)

func setupRoleSkills(t *testing.T, r *Runtime) {
	t.Helper()
	for name, roles := range map[string]string{"role-main": "[main]", "role-child": "[child]", "role-aside": "[aside]", "role-multi": "[child, aside]", "role-all": ""} {
		path := filepath.Join(r.Workspace.Root, ".agents", "skills", name, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		content := "plain unrestricted instructions"
		if roles != "" {
			content = "---\ndescription: useful metadata\nroles: " + roles + "\n---\nPRIVATE " + name + " instructions"
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := skills.Discover(context.Background(), r.Workspace.Root, "")
	if err != nil {
		t.Fatal(err)
	}
	r.Skills = c
	r.Tools = r.Tools.Filter(func(name string) bool { return name != "skill" })
	tool.AddSkills(r.Tools, c)
}

func assertRoleCatalog(t *testing.T, role string, messages []llm.Message) {
	t.Helper()
	catalogs := 0
	receiverCatalog := false
	for _, message := range messages {
		if !message.Type.IsRuntime() || !strings.Contains(message.Content, `"type":"runtime_context"`) {
			continue
		}
		var v runtimeContext
		if err := json.Unmarshal([]byte(message.Content), &v); err != nil {
			t.Fatal(err)
		}
		if v.Project == nil {
			continue
		}
		catalogs++
		actorRole, err := skills.RoleForActor(v.Actor)
		if err != nil {
			t.Fatal(err)
		}
		own := string(actorRole) == role
		receiverCatalog = receiverCatalog || own
		found := map[string]bool{}
		for _, s := range v.Project.Skills {
			if s.Content != "" {
				t.Fatal("advertised instruction content", s)
			}
			found[s.Name] = true
		}
		for _, candidate := range []string{"main", "child", "aside"} {
			if found["role-"+candidate] && role != candidate || own && found["role-"+candidate] != (role == candidate) {
				t.Fatal(role, "incorrect catalog from", v.Actor, found)
			}
		}
		if !found["role-all"] || role == "main" && found["role-multi"] || role != "main" && found["file-based-plan"] || own && (found["role-multi"] != (role != "main") || found["file-based-plan"] != (role == "main")) {
			t.Fatal(role, "incorrect default/multi/bundled catalog from", v.Actor, found)
		}
	}
	if catalogs == 0 || !receiverCatalog {
		t.Fatal("missing model-facing receiving skill catalog")
	}
}

func TestSkillRolesAtActualReceivingAndToolBoundaries(t *testing.T) {
	for _, role := range []string{"main", "child", "aside"} {
		t.Run(role, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			setupRoleSkills(t, r)
			requests := make(chan llm.Request, 3)
			calls := 0
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				requests <- req
				calls++
				if calls == 1 {
					assertRoleCatalog(t, role, req.Messages)
					name := "role-main"
					if role == "main" {
						name = "role-child"
					}
					call := llm.ToolCall{ID: "explicit-denial", Name: "skill", Arguments: []byte(`{"name":"` + name + `"}`)}
					return emit(llm.StreamEvent{Kind: "call", Call: &call})
				}
				found := false
				for _, message := range req.Messages {
					if message.Type != llm.TypeToolResult || message.CallID != "explicit-denial" {
						continue
					}
					found = true
					if !strings.Contains(message.Content, "skill_unavailable") || strings.Contains(message.Content, "PRIVATE") || strings.Contains(message.Content, r.Workspace.Root) {
						t.Error("denied load leaked/fell back", message.Content)
					}
				}
				if !found {
					t.Error("no explicit denial result at model boundary")
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Used only skills available to this role."})
			}}
			switch role {
			case "main":
				message := llm.Message{Type: llm.TypeUserInput, Content: "Review role skills"}
				if err := r.Run(&message); err != nil {
					t.Fatal(err)
				}
			case "child":
				args := `{"prompt":"Review role skills","label":"role audit","persistent":false}`
				turn, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "assign", Name: "subagent", Arguments: []byte(args)}})
				if result := childInvocation(t, r, ids[0], args); result["ok"] != true {
					t.Fatal(result)
				}
				if err := r.Store.FinishTurn(turn, "completed"); err != nil {
					t.Fatal(err)
				}
			case "aside":
				seedRuntime(t, r, "Review role skills")
				id, err := r.StartBTW("Review role skills")
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
			first, second := receive(t, requests), receive(t, requests)
			if role != "main" && first.ConversationID != second.ConversationID {
				t.Fatal("actor identity changed", first.ConversationID, second.ConversationID)
			}
		})
	}
}

func TestSkillRolesAsideInheritsRealMainAdmission(t *testing.T) {
	for _, active := range []bool{false, true} {
		name := "idle"
		if active {
			name = "active"
		}
		t.Run(name, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			setupRoleSkills(t, r)
			requests := make(chan llm.Request, 8)
			ready, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			mainCalls, asideCalls := 0, 0
			r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				requests <- req
				if !strings.Contains(req.ConversationID, "/btw_") {
					mainCalls++
					if mainCalls == 1 {
						call := llm.ToolCall{ID: "historical-load", Name: "skill", Arguments: []byte(`{"name":"role-main"}`)}
						return emit(llm.StreamEvent{Kind: "call", Call: &call})
					}
					close(ready)
					if active {
						select {
						case <-release:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					return emit(llm.StreamEvent{Kind: "text", Text: "Main evidence."})
				}
				asideCalls++
				if asideCalls == 1 {
					call := llm.ToolCall{ID: "aside-denial", Name: "skill", Arguments: []byte(`{"name":"role-main"}`)}
					return emit(llm.StreamEvent{Kind: "call", Call: &call})
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Aside answer."})
			}}
			done := make(chan error, 1)
			go func() {
				message := llm.Message{Type: llm.TypeUserInput, Content: "Main question evidence"}
				done <- r.Run(&message)
			}()
			select {
			case <-ready:
			case err := <-done:
				t.Fatal("main stopped before second admission", err)
			case <-time.After(3 * time.Second):
				t.Fatal("main admission did not finish")
			}
			if !active {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			assertRoleCatalog(t, "main", receive(t, requests).Messages)
			parent := receive(t, requests)
			assertRoleCatalog(t, "main", parent.Messages)
			before, err := r.Store.Messages(r.Current())
			if err != nil {
				t.Fatal(err)
			}
			id, err := r.StartBTW("Aside question")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			job, err := r.Jobs.Wait(ctx, "main", id, nil)
			if err != nil || job.Status != "completed" {
				t.Fatal(job, err)
			}
			for range 2 {
				request := receive(t, requests)
				assertRoleCatalog(t, "aside", request.Messages)
				inheritedCatalog, historicalLoad, userEvidence := false, false, false
				for _, message := range request.Messages {
					if message.Type.IsRuntime() && strings.Contains(message.Content, `"type":"runtime_context"`) {
						var runtime runtimeContext
						if err := json.Unmarshal([]byte(message.Content), &runtime); err != nil {
							t.Fatal(err)
						}
						inheritedCatalog = inheritedCatalog || runtime.Actor == "main" && runtime.Project != nil
					}
					historicalLoad = historicalLoad || message.CallID == "historical-load" && strings.Contains(message.Content, "PRIVATE role-main instructions")
					userEvidence = userEvidence || message.Content == "Main question evidence"
					if message.CallID == "aside-denial" && !strings.Contains(message.Content, "skill_unavailable") {
						t.Error("aside inherited permission to load main-only skill", message.Content)
					}
				}
				if !inheritedCatalog || !historicalLoad || !userEvidence {
					t.Fatal("lost inherited catalog provenance or historical evidence")
				}
			}
			after, err := r.Store.Messages(r.Current())
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("aside modified saved parent messages", err)
			}
			assertRoleCatalog(t, "main", parent.Messages)
			if active {
				r.Interrupt()
				<-done
			}
		})
	}
}

func TestSkillRolesInheritedProjectionPreservesOtherFields(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	setupRoleSkills(t, r)
	initial, _, err := r.runtimeContext(context.Background(), "main", r.selection, contextCursor{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(initial.Content), &fields); err != nil {
		t.Fatal(err)
	}
	fields["historical_extension"] = json.RawMessage(`{"evidence":"keep"}`)
	var project map[string]json.RawMessage
	if err := json.Unmarshal(fields["project"], &project); err != nil {
		t.Fatal(err)
	}
	project["historical_project_extension"] = json.RawMessage(`{"provenance":"keep"}`)
	fields["project"], err = json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	initial.Content = string(data)
	messages := []llm.Message{*initial, {Type: llm.TypeToolResult, CallID: "loaded-skill", Content: "PRIVATE role-main historical instructions"}, {Type: llm.TypeUserInput, Content: initial.Content}}
	before, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := r.projectInheritedSkillCatalogs(messages, "main/btw_projection")
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(messages)
	if err != nil || string(before) != string(after) {
		t.Fatal("projection mutated parent messages", err)
	}
	if !reflect.DeepEqual(messages[1:], projected[1:]) {
		t.Fatal("projection changed historical tool/user evidence")
	}
	var projectedFields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(projected[0].Content), &projectedFields); err != nil {
		t.Fatal(err)
	}
	var projectedProject map[string]json.RawMessage
	if err := json.Unmarshal(projectedFields["project"], &projectedProject); err != nil {
		t.Fatal(err)
	}
	delete(fields, "project")
	delete(projectedFields, "project")
	delete(project, "available_skills")
	delete(projectedProject, "available_skills")
	if !reflect.DeepEqual(fields, projectedFields) || !reflect.DeepEqual(project, projectedProject) {
		t.Fatal("projection changed non-catalog historical fields")
	}
}

func TestSkillRolesReannouncedAfterCompaction(t *testing.T) {
	for _, actor := range []string{"main", "main/child_skills", "main/btw_skills"} {
		t.Run(actor, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			setupRoleSkills(t, r)
			compactionBudget(t, r)
			seedCompactionHistory(t, r, strings.Repeat("Earlier evidence. ", 700))
			selection := r.selection
			initial, cursor, err := r.runtimeContext(context.Background(), actor, selection, contextCursor{})
			if err != nil {
				t.Fatal(err)
			}
			role, err := skills.RoleForActor(actor)
			if err != nil {
				t.Fatal(err)
			}
			assertRoleCatalog(t, string(role), []llm.Message{*initial})
			r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
				return emit(llm.StreamEvent{Kind: "text", Text: "Earlier evidence summarized."})
			}}
			if actor == "main" {
				r.mainContext = cursor
				_, err = r.compactContext(context.Background(), "", selection, nil)
				cursor = r.mainContext
			} else {
				turn, beginErr := r.Store.BeginChildTurn(r.Current(), actor, selection)
				if beginErr != nil {
					t.Fatal(beginErr)
				}
				task := childTask{actor: actor, turn: turn, selection: selection, tools: r.Tools, aside: role == skills.Aside}
				messages := []llm.Message{{Type: llm.TypeUserInput, Content: "Review", InputTimeMS: time.Now().UnixMilli()}, {Type: llm.TypeAssistant, Content: strings.Repeat("Earlier evidence. ", 700)}}
				seedIsolatedHistory(t, r, actor, turn, messages)
				_, cursor, err = r.compactChild(context.Background(), task, messages, cursor, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			message, _, err := r.runtimeContext(context.Background(), actor, selection, cursor)
			if err != nil || message == nil {
				t.Fatal(message, err)
			}
			assertRoleCatalog(t, string(role), []llm.Message{*message})
		})
	}
}

func TestSkillCatalogRejectsUnknownRuntimeActor(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	if message, _, err := r.runtimeContext(context.Background(), "child", r.selection, contextCursor{}); err == nil || message != nil {
		t.Fatal(message, err)
	}
}

func TestSkillCatalogFailedAdmissionRetries(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	setupRoleSkills(t, r)
	seedRuntime(t, r, "Review skill catalog")
	turn := admissionTurn(t, r)
	before := r.mainContext
	if _, err := r.Store.DB.Exec(`CREATE TRIGGER fail_skill_admission BEFORE INSERT ON model_requests BEGIN SELECT RAISE(ABORT,'forced admission failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.admitMain(context.Background(), turn, r.selection); err == nil {
		t.Fatal("admission unexpectedly succeeded")
	}
	if !reflect.DeepEqual(before, r.mainContext) {
		t.Fatal("failed admission advanced catalog cursor")
	}
	if _, err := r.Store.DB.Exec("DROP TRIGGER fail_skill_admission"); err != nil {
		t.Fatal(err)
	}
	_, message, err := r.admitMain(context.Background(), turn, r.selection)
	if err != nil || message == nil {
		t.Fatal(message, err)
	}
	assertRoleCatalog(t, "main", []llm.Message{*message})
}
