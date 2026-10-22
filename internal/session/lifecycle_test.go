package session

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"ttc/internal/capture"
	contextbuild "ttc/internal/context"
	"ttc/internal/history"
	"ttc/internal/historytest"
	"ttc/internal/llm"
	"ttc/internal/tool"
)

func TestExplicitLoadEntryPointsSharePreparedSelection(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(map[bool]string{false: "writable", true: "archived"}[archived], func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			seedRuntime(t, r, "Saved context")
			source := r.Current()
			original := r.CurrentSelection()
			if archived {
				if _, err := historytest.DB(t, r.Store).Exec("UPDATE sessions SET read_only=1 WHERE id=?", source); err != nil {
					t.Fatal(err)
				}
			}
			next := original
			next.Variant = "high"
			r.selection = next
			startup, err := New(context.Background(), r.Store, r.Workspace, r.Provider, next, "", r.Skills, tool.WebSearchConfig{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(startup.Close)
			if err := startup.Load(source); err != nil { // The CLI startup path.
				t.Fatal(err)
			}
			if _, err := r.Command("/load " + source); err != nil {
				t.Fatal(err)
			}
			want := next
			if archived {
				want = original
			}
			if !reflect.DeepEqual(startup.CurrentSelection(), want) || !reflect.DeepEqual(r.CurrentSelection(), want) {
				t.Fatal("entry points disagree on selection", startup.CurrentSelection(), r.CurrentSelection(), want)
			}
			if !reflect.DeepEqual(startup.UsageSnapshot(), r.UsageSnapshot()) {
				t.Fatal("entry points disagree on restored accounting", startup.UsageSnapshot(), r.UsageSnapshot())
			}
			if archived && (startup.Current() != source || r.Current() != source) {
				t.Fatal("archived history was copied")
			}
			if !archived && (startup.Current() == source || r.Current() == source || startup.Current() == r.Current()) {
				t.Fatal("writable history did not get independent snapshots")
			}
			var count int
			if err := historytest.DB(t, r.Store).QueryRow("SELECT count(*) FROM sessions").Scan(&count); err != nil {
				t.Fatal(err)
			}
			wantCount := 3 // Source plus exactly one copy per entry point.
			if archived {
				wantCount = 1
			}
			if count != wantCount {
				t.Fatalf("session copies: got %d, want %d", count, wantCount)
			}
		})
	}
}

func TestLoadPreparationFailureLeavesLiveWorkRunning(t *testing.T) {
	for _, failure := range []string{"workspace", "projection", "switch", "activity"} {
		t.Run(failure, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			seedRuntime(t, r, "Active context")
			source := history.NewID("session")
			selection := r.CurrentSelection()
			selection.Variant = "other"
			root := r.Workspace.Root
			if failure == "workspace" {
				root = t.TempDir()
			}
			turn, _, err := r.Store.StartSession(source, root, selection, llm.Message{Type: llm.TypeUserInput, Content: "Load target"})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Store.FinishTurn(turn, "completed"); err != nil {
				t.Fatal(err)
			}
			job, err := r.Jobs.Start("main", "sleep 30", r.Workspace.Root, 0, true, false, false, capture.OutputOptions{}, "keep running")
			if err != nil {
				t.Fatal(err)
			}
			wake, err := r.timers.schedule(wakeupSchedule{Name: "keep scheduled", Message: "later"}, "main", time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			id, generation, manager, timers, naming, estimator := r.Current(), r.Generation(), r.Jobs, r.timers, r.namingCtx, r.estimator
			before := r.UsageSnapshot()
			db := historytest.DB(t, r.Store)
			switch failure {
			case "projection":
				_, err = db.Exec(`UPDATE entries SET content_json=json_set(content_json,'$.input_source','invalid') WHERE session_id=? AND role='user'`, source)
			case "switch":
				_, err = db.Exec(`CREATE TRIGGER reject_load_switch BEFORE UPDATE OF model_json ON sessions BEGIN SELECT RAISE(ABORT,'switch blocked'); END`)
			case "activity":
				_, err = db.Exec(`CREATE TRIGGER reject_load_activity BEFORE UPDATE OF last_activity_ms ON sessions BEGIN SELECT RAISE(ABORT,'activity blocked'); END`)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Load(source); err == nil {
				t.Fatal("invalid preparation succeeded")
			}
			if r.Current() != id || r.Generation() != generation || r.Jobs != manager || r.timers != timers || r.namingCtx != naming || naming.Err() != nil || r.estimator != estimator || !reflect.DeepEqual(r.UsageSnapshot(), before) {
				t.Fatal("failed preparation replaced active runtime state")
			}
			view, err := r.Jobs.View("main", job)
			if err != nil || view.Status != "running" {
				t.Fatal("failed preparation stopped job", view, err)
			}
			select {
			case <-wake.done:
				t.Fatal("failed preparation stopped reminder")
			default:
			}
		})
	}
}

func TestPreparedActivationDoesNotReadStorage(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Frozen target")
	target, err := r.prepareSession(r.Current(), true)
	if err != nil {
		t.Fatal(err)
	}
	// Accounting is fully prepared, not reconstructed during activation.
	if err := r.Store.Close(); err != nil {
		t.Fatal(err)
	}
	r.activateSession(target)
	if r.Current() != target.session.ID || r.estimator != target.accounting.estimator {
		t.Fatal("activation did not install prepared state")
	}
}

func TestLoadAccountingUsesActivatedToolSchemas(t *testing.T) {
	for _, mode := range []string{"extended", "replaced", "mutated-schema"} {
		t.Run(mode, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			seedRuntime(t, r, "Saved context")
			source := r.Current()
			messages, err := r.Store.Messages(source)
			if err != nil {
				t.Fatal(err)
			}
			definitions := r.Tools.Definitions()
			for i := range definitions {
				definitions[i].Parameters = append([]byte(nil), definitions[i].Parameters...)
			}
			switch mode {
			case "replaced":
				r.Tools = tool.NewRegistry()
			case "mutated-schema":
				// Definitions returns value copies with shared schema bytes.
				// Mutating that public view must not alter frozen defaults.
				active := r.Tools.Definitions()
				active[0].Parameters[0] = ' '
				if !reflect.DeepEqual(r.defaultTools, definitions) {
					t.Fatal("default snapshot aliases active schema bytes")
				}
			default:
				tool.Register(r.Tools, "custom", strings.Repeat("extra schema text ", 1000), nil, nil, nil,
					func(context.Context, tool.Execution, struct{}) (any, error) { return nil, nil })
			}
			if err := r.Load(source); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(r.Tools.Definitions(), definitions) {
				t.Fatal("activation did not rebuild the default registry")
			}
			// No historical measurement exists, so the prepared sidebar estimate
			// must match the exact schemas now advertised by the fresh runtime.
			estimate := contextbuild.EstimateInput(contextbuild.InputSnapshot{
				Selection: r.CurrentSelection(), System: systemTemplate, Tools: r.Tools.Definitions(),
				Messages: messages, Lineage: r.Current(), TokenEstimator: r.Provider,
			})
			if got := r.UsageSnapshot(); got.Input != estimate.Input {
				t.Fatalf("prepared accounting used source schemas: got %d, want %d", got.Input, estimate.Input)
			}
		})
	}
}
