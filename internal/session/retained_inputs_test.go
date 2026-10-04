package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/provider"
)

// assertRetainedProjection compares the complete canonical replacement, not
// just human text. The persisted marker supplies the exact frozen cut time.
func assertRetainedProjection(t *testing.T, original, got []provider.Message, selection provider.Selection) {
	t.Helper()
	canonical := canonicalCompaction(original)
	retention, err := contextbuild.Retain(canonical, selection.Model.Budget.RecentTokensMin, selection.Model.Budget.RecentTokensMax)
	if err != nil || len(retention.Inputs) != 4 || len(got) < 2 {
		t.Fatal("invalid four-input projection", retention, got, err)
	}
	var metadata struct {
		At int64 `json:"compaction_at_ms"`
	}
	marker := got[1].Content
	if err := json.Unmarshal([]byte(marker[strings.LastIndex(marker, "\n")+1:]), &metadata); err != nil || metadata.At <= 0 {
		t.Fatal("invalid retained marker", marker, err)
	}
	inputs, err := contextbuild.RetainedInputMessages(canonical, retention.Inputs, time.UnixMilli(metadata.At))
	if err != nil {
		t.Fatal(err)
	}
	want := append([]provider.Message{got[0]}, inputs...)
	want = append(want, canonical[retention.Start:]...)
	if !strings.HasPrefix(got[0].Content, "Policy handoff.") || !reflect.DeepEqual(got, want) {
		t.Fatalf("assembled and persisted projections differ:\ngot %#v\nwant %#v", got, want)
	}
}

func TestIsolatedInputTimestampUsesCommittedEntry(t *testing.T) {
	for _, aside := range []bool{false, true} {
		t.Run(fmt.Sprint(aside), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			seedRuntime(t, r, "main task")
			task := childTask{actor: "main/timestamp", prompt: "committed input", selection: r.selection, tools: r.Tools, aside: aside}
			var err error
			task.turn, err = r.Store.BeginChildTurn(r.Current(), task.actor, task.selection)
			if err != nil {
				t.Fatal(err)
			}
			var held *sql.Tx
			ready := make(chan struct{})
			hold := func() {
				var err error
				held, err = r.Store.DB.Begin()
				if err != nil {
					t.Error(err)
				}
				close(ready)
			}
			if !aside {
				hold()
			}
			r.Emit = func(event Event) {
				if aside && event.Text == "Aside instructions · inspect" {
					hold()
				}
			}
			r.Provider = &childProvider{stream: func(_ context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
				var committed int64
				if err := r.Store.DB.QueryRow("SELECT created_ms FROM entries WHERE actor_id=? AND role='user' ORDER BY id LIMIT 1", task.actor).Scan(&committed); err != nil {
					return err
				}
				found := 0
				for _, message := range req.Messages {
					if message.Content == task.prompt {
						found++
						source := "task"
						if aside {
							source = "btw"
						}
						if message.Role != "user" || message.Runtime || message.InputSource != source || message.InputTimeMS != committed {
							return fmt.Errorf("isolated input provenance differs from committed entry: %#v, want %s at %d", message, source, committed)
						}
					}
				}
				if found != 1 {
					return fmt.Errorf("isolated input occurred %d times, want once", found)
				}
				return emit(provider.StreamEvent{Kind: "text", Text: "Finished."})
			}}
			done := make(chan error, 1)
			go func() { done <- r.runChild(context.Background(), task, io.Discard, io.Discard) }()
			<-ready
			// Force admission to wait across several clock ticks. A pre-admission
			// timestamp is observably different from the eventual persisted one.
			time.Sleep(30 * time.Millisecond)
			if held != nil {
				if err := held.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("isolated input did not finish")
			}
		})
	}
}

func TestMainRetainedInputsPolicyAcrossCompactions(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprint(automatic), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			seedRuntime(t, r, "oldest ordinary")
			r.selection.Model.Budget.RecentTokensMin = 0
			r.selection.Model.Budget.RecentTokensMax = 700
			r.selection.Model.Budget.ContextLimit = 1 << 20
			originalIDs := map[string]int64{}
			admit := func(source, text string) {
				t.Helper()
				message := (contextbuild.Input{Text: "  " + text + "\n\tλ  ", Source: source, Attachments: []contextbuild.Attachment{
					{Kind: "text", Path: "notes.txt", Text: "immutable original\n"},
				}}).Message()
				// One real image snapshot is enough to catch canonical-copy loss.
				if text == "ordinary two" {
					message.Images = []provider.Image{{Path: "original.png", DataURL: "data:image/png;base64,b3JpZ2luYWw="}}
				}
				var turn string
				var id int64
				var err error
				if source == "steer" {
					turn, err = r.Store.BeginTurn(r.Current(), "steer", r.selection)
					if err == nil {
						id, err = r.Store.Append(r.Current(), turn, "main", "message", "user", true, message)
					}
				} else {
					turn, id, err = r.Store.AdmitTurn(r.Current(), "user", r.selection, &message)
				}
				if err != nil {
					t.Fatal(err)
				}
				originalIDs[message.Content] = id
				if err := r.Store.FinishTurn(turn, "completed"); err != nil {
					t.Fatal(err)
				}
			}
			for _, input := range [][2]string{{"steer", "old steer"}, {"queue", "old ordinary"}, {"steer", "steer one"}, {"normal", "ordinary one"}, {"steer", "steer two"}, {"queue", "ordinary two"}} {
				admit(input[0], input[1])
			}
			appendMessage := func(message provider.Message, visible bool) {
				t.Helper()
				if _, err := r.Store.Append(r.Current(), "", "main", "message", message.Role, visible, message); err != nil {
					t.Fatal(err)
				}
			}
			for iteration := range 2 {
				if iteration > 0 {
					admit("queue", "new ordinary")
					admit("steer", "new steer")
				}
				appendMessage(provider.Message{Role: "assistant", Content: strings.Repeat("completed older work ", 1200)}, true)
				appendMessage(provider.Message{Role: "user", Runtime: true, InputSource: "steer", Content: "runtime notice is not a steer"}, true)
				appendMessage(provider.Message{Role: "assistant", Calls: []provider.ToolCall{{ID: "a", Name: "read", Arguments: []byte(`{}`)}, {ID: "b", Name: "read", Arguments: []byte(`{}`)}}, State: &provider.ReplayState{Provider: "fixture", Model: "fixture", Version: 1, Items: []json.RawMessage{json.RawMessage(`{"native":true}`)}}}, true)
				appendMessage(provider.Message{Role: "tool", CallID: "b", Content: "second result first"}, true)
				appendMessage(provider.Message{Role: "tool", CallID: "a", Content: "first result second"}, true)
				appendMessage(provider.Message{Role: "assistant", Content: "UI-only child output"}, false)
				original, err := r.Store.Messages(r.Current())
				if err != nil {
					t.Fatal(err)
				}
				if automatic {
					fresh, _, err := r.runtimeContext(context.Background(), "main", r.selection, contextCursor{})
					if err != nil {
						t.Fatal(err)
					}
					usage := estimateUsage(r.selection, systemTemplate, r.Tools.Definitions(), append(canonicalCompaction(original), *fresh))
					r.selection.Model.Budget.ContextLimit = usage.Input + usage.Reserved - 1000
				}
				selection := r.selection
				var replacement []provider.Message
				assertImmediateOccupancy := func() {
					t.Helper()
					r.orderMu.Lock()
					pending := append(append([]provider.Message(nil), r.notifications...), r.steeringMessagesLocked()...)
					fresh, _, err := r.runtimeContextLocked(context.Background(), "main", selection, r.mainContext)
					r.orderMu.Unlock()
					if err != nil {
						t.Fatal(err)
					}
					input := append(append([]provider.Message(nil), replacement...), pending...)
					if fresh != nil {
						input = append(input, *fresh)
					}
					want := estimateUsage(selection, systemTemplate, r.Tools.Definitions(), input)
					got := r.UsageSnapshot()
					if got.Input != want.Input || got.Reserved != want.Reserved || got.Limit != want.Limit {
						t.Fatalf("budgeted assembly omitted persisted replacement/pending/context: got %+v, want %+v", got, want)
					}
					if !contextbuild.Fits(selection, systemTemplate, r.Tools.Definitions(), input, true) {
						t.Fatal("persisted replacement does not fit complete handoff budget")
					}
				}
				r.Emit = func(event Event) {
					if event.Kind == "continuation" {
						var err error
						replacement, err = r.Store.Messages(r.Current())
						if err != nil {
							t.Error(err)
						}
						assertImmediateOccupancy()
					}
				}
				summaries := 0
				var tailID int64
				r.Provider = &childProvider{stream: func(_ context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
					if req.NoTools {
						summaries++
						var err error
						tailID, err = r.Store.Append(r.Current(), "", "main/child", "status", "", false, map[string]string{"text": "committed invisible tail"})
						if err != nil {
							return err
						}
						if !automatic {
							r.orderMu.Lock()
							r.steers = []contextbuild.Input{{Text: "pending one"}, {Text: "pending two"}, {Text: "pending three"}}
							r.orderMu.Unlock()
						}
						return emit(provider.StreamEvent{Kind: "text", Text: "Policy handoff."})
					}
					return emit(provider.StreamEvent{Kind: "text", Text: "Continued."})
				}}
				if automatic {
					err = r.Run(nil)
				} else {
					_, err = r.Command("/compact")
					replacement, _ = r.Store.Messages(r.Current())
					assertImmediateOccupancy()
					for _, text := range []string{"pending three", "pending two", "pending one"} {
						input, cancelErr := r.CancelSteer()
						if cancelErr != nil || input.Text != text {
							t.Fatal("pending state was admitted or changed during handoff", input, cancelErr)
						}
					}
				}
				if err != nil || summaries != 1 {
					t.Fatal("compaction did not complete once", err, summaries)
				}
				assertRetainedProjection(t, original, replacement, selection)
				entries, err := r.Store.Branch(r.Current(), 0)
				if err != nil {
					t.Fatal(err)
				}
				foundTail := false
				for _, entry := range entries {
					foundTail = foundTail || entry.EventSeq() == tailID
					if entry.Role == "user" && entry.Visible {
						var message provider.Message
						if err := json.Unmarshal(entry.Content, &message); err != nil {
							t.Fatal(err)
						}
						if !message.Runtime && entry.EventSeq() != originalIDs[message.Content] {
							t.Fatal("original admission identity changed", entry)
						}
					}
				}
				if !foundTail {
					t.Fatal("invisible committed tail was lost")
				}
			}
		})
	}
}

func TestIsolatedRetainedInputsPolicyAcrossCompactions(t *testing.T) {
	for _, aside := range []bool{false, true} {
		t.Run(fmt.Sprint(aside), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			seedRuntime(t, r, "main unchanged")
			task := childTask{actor: "main/child_policy", selection: r.selection, tools: r.Tools, aside: aside}
			var err error
			task.turn, err = r.Store.BeginChildTurn(r.Current(), task.actor, task.selection)
			if err != nil {
				t.Fatal(err)
			}
			r.Provider = &childProvider{stream: func(_ context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
				return emit(provider.StreamEvent{Kind: "text", Text: "Policy handoff."})
			}}
			var messages []provider.Message
			for i, source := range []string{"task", "steer", "queue", "steer", "btw", "steer"} {
				messages = append(messages, provider.Message{Role: "user", InputSource: source, InputTimeMS: time.Now().Add(-time.Duration(10-i) * time.Minute).UnixMilli(), Content: fmt.Sprintf("original %d", i)})
			}
			for range 2 {
				messages = append(messages, provider.Message{Role: "assistant", Content: strings.Repeat("older work ", 3000)}, provider.Message{Role: "assistant", Content: "recent work"})
				result, _, err := r.compactChild(context.Background(), task, messages, contextCursor{})
				if err != nil {
					t.Fatal(err)
				}
				projection := result
				if aside {
					if result[1].Content != btwInstruction {
						t.Fatal("aside policy lost")
					}
					projection = append([]provider.Message{result[0]}, result[2:]...)
				}
				assertRetainedProjection(t, messages, projection, task.selection)
				messages = result
			}
		})
	}
}
