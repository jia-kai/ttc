package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/provider"
)

func TestCompactionIgnoresUIOnlyHistoryButArchivesIt(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	compactionBudget(t, r)
	seedCompactionHistory(t, r, strings.Repeat("Old coding facts. ", 700))
	before := r.Current()
	hidden := strings.Repeat("UI_ONLY_CHILD_DIAGNOSTIC ", 10000)
	if _, err := r.Store.Append(before, "", "main/child_fixture", "message", "assistant", false, provider.Message{Role: "assistant", Content: hidden}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.Append(before, "", "main", "message", "user", true, provider.Message{Role: "user", Content: "Continue."}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	r.Provider = &childProvider{stream: func(_ context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
		calls++
		input := req.Messages[0].Content
		if !strings.Contains(input, "Old coding facts.") || strings.Contains(input, "UI_ONLY_CHILD_DIAGNOSTIC") {
			t.Fatal("summary does not match main model projection")
		}
		return emit(provider.StreamEvent{Kind: "text", Text: "Concise handoff."})
	}}
	if _, err := r.compactContext(context.Background(), "", r.CurrentSelection()); err != nil || calls != 1 || r.Current() == before {
		t.Fatal("UI-only records prevented handoff", err, calls)
	}
	files, err := filepath.Glob(filepath.Join(r.Store.Root, "lineages", before, "compactions", "*"))
	if err != nil || len(files) != 2 {
		t.Fatal("missing complete archives", files, err)
	}
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil || !strings.Contains(string(body), hidden) {
			t.Fatal("archive lost child history", file, err)
		}
	}
}

func TestCompactionKeepsRawToolPayloadsForMainAndChild(t *testing.T) {
	for _, child := range []bool{false, true} {
		t.Run(fmt.Sprint(child), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			compactionBudget(t, r)
			seedRuntime(t, r, "Research arrays.")
			payload := "[" + strings.Repeat("0,", 7000) + "0]"
			messages := []provider.Message{
				{Role: "user", Content: "Inspect this array.", InputTimeMS: time.Now().Add(-time.Minute).UnixMilli()},
				{Role: "assistant", Calls: []provider.ToolCall{{ID: "array", Name: "read", Arguments: json.RawMessage(`{"path":"data.json"}`)}}},
				{Role: "tool", CallID: "array", Content: payload},
				{Role: "user", Content: "Continue.", InputTimeMS: time.Now().Add(-time.Second).UnixMilli()},
			}
			calls := 0
			r.Provider = &childProvider{stream: func(_ context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
				calls++
				if !strings.Contains(req.Messages[0].Content, payload) || !strings.Contains(req.Messages[0].Content, `{"path":"data.json"}`) {
					t.Fatal("tool payload was expanded or omitted")
				}
				return emit(provider.StreamEvent{Kind: "text", Text: "Array inspected."})
			}}
			var err error
			if child {
				actor := "main/child_array"
				turn, e := r.Store.BeginChildTurn(r.Current(), actor, r.CurrentSelection())
				if e != nil {
					t.Fatal(e)
				}
				_, _, err = r.compactChild(context.Background(), childTask{actor: actor, turn: turn, selection: r.CurrentSelection(), tools: r.Tools}, messages, contextCursor{})
			} else {
				for _, message := range messages {
					if _, e := r.Store.Append(r.Current(), "", "main", "message", message.Role, true, message); e != nil {
						t.Fatal(e)
					}
				}
				_, err = r.compactContext(context.Background(), "", r.CurrentSelection())
			}
			if err != nil || calls != 1 {
				t.Fatal("raw tool result did not fit", err, calls)
			}
		})
	}
}

func TestCompactionSummarizesOversizedRecentToolCycle(t *testing.T) {
	for _, actor := range []string{"main", "child", "btw"} {
		t.Run(actor, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			compactionBudget(t, r)
			seedRuntime(t, r, "Earlier research.")
			payload := strings.Repeat("large recent tool output ", 100)
			messages := []provider.Message{
				{Role: "user", Content: "Earlier task", InputTimeMS: time.Now().Add(-time.Minute).UnixMilli()},
				{Role: "assistant", Content: "Earlier result"},
				{Role: "user", Content: "Continue the current task", InputTimeMS: time.Now().Add(-time.Second).UnixMilli()},
				{Role: "assistant", Calls: []provider.ToolCall{{ID: "read", Name: "read", Arguments: []byte(`{"path":"fixture.txt"}`)}}},
				{Role: "tool", CallID: "read", Content: payload},
			}
			requests := 0
			r.Provider = &childProvider{stream: func(_ context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
				requests++
				if !req.NoTools || !strings.Contains(req.Messages[0].Content, payload) || !strings.Contains(req.Messages[0].Content, `{"path":"fixture.txt"}`) {
					t.Fatal("summary lost the complete oversized cycle")
				}
				return emit(provider.StreamEvent{Kind: "text", Text: "Recent output was inspected; continue the task."})
			}}
			var result []provider.Message
			var err error
			if actor == "main" {
				for _, message := range messages {
					if _, err := r.Store.Append(r.Current(), "", "main", "message", message.Role, true, message); err != nil {
						t.Fatal(err)
					}
				}
				if _, err = r.Command("/compact"); err == nil {
					result, err = r.Store.Messages(r.Current())
				}
			} else {
				id := "main/" + actor
				turn, e := r.Store.BeginChildTurn(r.Current(), id, r.CurrentSelection())
				if e != nil {
					t.Fatal(e)
				}
				result, _, err = r.compactChild(context.Background(), childTask{actor: id, turn: turn, selection: r.CurrentSelection(), tools: r.Tools, aside: actor == "btw"}, messages, contextCursor{})
			}
			if err != nil || requests != 1 || r.checkContext() != nil {
				t.Fatal("oversized cycle prevented handoff", err, requests)
			}
			foundInstruction := false
			for _, message := range result {
				if message.Content == messages[2].Content {
					foundInstruction = true
				}
				if len(message.Calls) > 0 || message.Role == "tool" || message.Content == payload {
					t.Fatal("oversized cycle survived the hard maximum")
				}
			}
			if !foundInstruction {
				t.Fatal("human instruction was not retained")
			}
		})
	}
}

func TestCompactionRejectsGenuinelyOversizedPrefixWithoutRequest(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	compactionBudget(t, r)
	seedCompactionHistory(t, r, strings.Repeat("Actual model history. ", 10000))
	before := r.Current()
	if _, err := r.Store.Append(before, "", "main", "message", "user", true, provider.Message{Role: "user", Content: "Continue."}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	r.Provider = &childProvider{stream: func(context.Context, provider.Request, func(provider.StreamEvent) error) error { calls++; return nil }}
	_, err := r.compactContext(context.Background(), "", r.CurrentSelection())
	if err == nil || !strings.Contains(err.Error(), "estimated input") || calls != 0 || r.Current() != before || r.checkContext() == nil {
		t.Fatal("oversized canonical input was admitted", err, calls)
	}
	files, err := filepath.Glob(filepath.Join(r.Store.Root, "lineages", before, "compactions", "*"))
	if err != nil || len(files) != 2 {
		t.Fatal("failed input was not archived", files, err)
	}
}

func TestChildArchivePreservesModelVisibleJobOutput(t *testing.T) {
	messages := []provider.Message{
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "shell", Name: "shell", Arguments: json.RawMessage(`{"command":"printf diagnostic"}`)}}},
		{Role: "tool", CallID: "shell", Content: `{"job_id":"job_fixture","stdout":"diagnostic sentinel","stderr":"warning sentinel","status":"completed"}`},
	}
	text := childTranscript(messages)
	for _, value := range []string{"diagnostic sentinel", "warning sentinel", "printf diagnostic", "stdout", "stderr"} {
		if !strings.Contains(text, value) {
			t.Fatal("child archive lost tool information", value, text)
		}
	}
}

func TestCompactionFailureClassification(t *testing.T) {
	transient := &provider.TransientError{Err: errors.New("service unavailable")}
	for _, test := range []struct {
		name        string
		err         error
		recoverable bool
	}{
		{"cancel", context.Canceled, true}, {"timeout", context.DeadlineExceeded, true},
		{"transport", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, true},
		{"service", transient, true}, {"disk full", &os.PathError{Op: "write", Path: "archive", Err: syscall.ENOSPC}, true},
		{"malformed", errors.New("invalid summary"), false},
		{"joined storage", fmt.Errorf("save summary: %w", errors.Join(transient, errors.New("corrupt history"))), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := recoverableCompaction(test.err); got != test.recoverable {
				t.Fatalf("recoverable=%v want %v", got, test.recoverable)
			}
		})
	}
}

func TestCompactionFatalAndRecoverableReload(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		t.Run(fmt.Sprint(fatal), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			compactionBudget(t, r)
			seedCompactionHistory(t, r, strings.Repeat("Previous research notes. ", 700))
			if _, err := r.Store.Append(r.Current(), "", "main", "message", "user", true, provider.Message{Role: "user", Content: "Continue the research."}); err != nil {
				t.Fatal(err)
			}
			before := r.Current()
			requests := 0
			r.Provider = &childProvider{stream: func(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
				requests++
				if fatal {
					return emit(provider.StreamEvent{Kind: "text", Text: "   "})
				}
				return &provider.TransientError{Err: errors.New("fixture transport interrupted")}
			}}
			_, err := r.compactContext(context.Background(), "", r.CurrentSelection())
			if err == nil || requests != 1 || r.Current() != before {
				t.Fatal("unexpected handoff", err, requests, r.Current())
			}
			loaded, err := r.Store.Load(before)
			if err != nil {
				t.Fatal(err)
			}
			if (loaded.CompactionError != "") != fatal || loaded.ReadOnly {
				t.Fatal("invalid persisted context policy", loaded)
			}
			if got := r.checkContext(); (got != nil) != fatal {
				t.Fatal("runtime guard", got)
			}
			if fatal {
				if err := r.Run(&provider.Message{Role: "user", Content: "continue"}); err == nil {
					t.Fatal("fatal context admitted inference")
				}
				if requests != 1 {
					t.Fatal("fatal context contacted provider", requests)
				}
			}
		})
	}
}

func TestCompactionRejectsOversizedPendingSteerBeforeHandoff(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	compactionBudget(t, r)
	seedCompactionHistory(t, r, strings.Repeat("Earlier notes. ", 700))
	if _, err := r.Store.Append(r.Current(), "", "main", "message", "user", true, provider.Message{Role: "user", Content: "Continue."}); err != nil {
		t.Fatal(err)
	}
	before := r.Current()
	summaries := 0
	r.Provider = &childProvider{stream: func(_ context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
		summaries++
		// A user may steer while the summarizer is running. Admission must not
		// repeatedly compact a handoff that still cannot fit that instruction.
		r.orderMu.Lock()
		r.steers = append(r.steers, contextbuild.Input{Text: strings.Repeat("pending research instruction ", 3000)})
		r.orderMu.Unlock()
		return emit(provider.StreamEvent{Kind: "text", Text: "Concise handoff."})
	}}
	_, err := r.compactContext(context.Background(), "", r.CurrentSelection())
	if err == nil || !strings.Contains(err.Error(), "exceed context headroom") || r.Current() != before || summaries != 1 {
		t.Fatal("oversized steer caused an unusable handoff", err, r.Current(), summaries)
	}
	if r.checkContext() == nil {
		t.Fatal("unfittable compaction was not classified as fatal")
	}
}

func TestChildCompactionIsolatedArchiveAndNotification(t *testing.T) {
	for _, aside := range []bool{false, true} {
		t.Run(fmt.Sprint(aside), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			compactionBudget(t, r)
			seedRuntime(t, r, "Main context stays untouched.")
			before, err := r.CurrentSession()
			if err != nil {
				t.Fatal(err)
			}
			main, err := r.Store.Messages(before.ID)
			if err != nil {
				t.Fatal(err)
			}
			originalMain, _ := json.Marshal(main)
			actor := "main/child_fixture"
			if aside {
				actor = "main/btw_fixture"
			}
			turn, err := r.Store.BeginChildTurn(before.ID, actor, r.CurrentSelection())
			if err != nil {
				t.Fatal(err)
			}
			messages := []provider.Message{
				{Role: "user", Content: "Earlier child task", InputTimeMS: time.Now().Add(-time.Minute).UnixMilli()},
				{Role: "assistant", Content: strings.Repeat("Old child research notes. ", 350)},
				{Role: "user", Content: "Read the fixture", InputTimeMS: time.Now().Add(-time.Second).UnixMilli()},
				{Role: "assistant", Calls: []provider.ToolCall{{ID: "read_fixture", Name: "read", Arguments: json.RawMessage(`{"path":"fixture.go"}`)}}, State: &provider.ReplayState{Provider: "script", Model: "fixture", Version: 1, Items: []json.RawMessage{json.RawMessage(`{"private":"replay"}`)}}},
				{Role: "tool", CallID: "read_fixture", Content: `{"content":"package fixture"}`},
			}
			exactOriginal, _ := json.Marshal(messages)
			r.Provider = &childProvider{stream: func(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
				if !req.NoTools || len(req.Tools) != 0 || !strings.Contains(req.Messages[0].Content, "Old child research notes") {
					t.Fatal("invalid summarization request", req)
				}
				return emit(provider.StreamEvent{Kind: "text", Text: "Useful child handoff."})
			}}
			task := childTask{actor: actor, turn: turn, selection: r.CurrentSelection(), tools: r.Tools, aside: aside}
			result, cursor, err := r.compactChild(context.Background(), task, messages, contextCursor{project: "old project", snapshot: "old runtime"})
			if err != nil {
				t.Fatal(err)
			}
			expected := 7
			if aside {
				expected = 8
			}
			if cursor.project != "" || cursor.snapshot != "" || len(result) != expected || !strings.HasPrefix(result[0].Content, "Useful child handoff.") {
				t.Fatal("incorrect retained child projection", cursor, result)
			}
			for _, message := range result {
				if message.State != nil {
					t.Fatal("opaque replay survived compaction")
				}
				if aside && result[1].Content != btwInstruction {
					t.Fatal("aside scope lost during compaction")
				}
				var replyEntry int64
				if err := r.Store.DB.QueryRow(`SELECT id FROM entries WHERE actor_id=? AND json_extract(content_json,'$.type')='request_message' AND json_extract(content_json,'$.purpose')='compaction' AND json_extract(content_json,'$.role')='assistant' ORDER BY id DESC LIMIT 1`, actor).Scan(&replyEntry); err != nil {
					t.Fatal("compaction reply lost actor/purpose", err)
				}
				replyRecord, err := r.Store.Entry(replyEntry)
				if err != nil {
					t.Fatal(err)
				}
				inspected, err := r.Store.Inspect(replyRecord)
				if err != nil || inspected != "Useful child handoff." {
					t.Fatal("compaction reply is not Markdown", inspected, err)
				}
			}
			originalNow, _ := json.Marshal(messages)
			if string(originalNow) != string(exactOriginal) {
				t.Fatal("compaction mutated caller input")
			}
			after, err := r.CurrentSession()
			if err != nil {
				t.Fatal(err)
			}
			currentMain, _ := r.Store.Messages(before.ID)
			encodedMain, _ := json.Marshal(currentMain)
			if after.ID != before.ID || after.ReadOnly || after.FileTip != before.FileTip || after.UndoFloor != before.UndoFloor || string(encodedMain) != string(originalMain) {
				t.Fatal("child changed parent context or undo state", after)
			}
			archiveLine := strings.Split(strings.Split(result[0].Content, "Earlier history: ")[1], "\n")[0]
			exact, err := os.ReadFile(archiveLine + ".jsonl")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(exact), `"private":"replay"`) || !strings.Contains(string(exact), actor) {
				t.Fatal("missing exact actor replay", string(exact))
			}
			md, err := os.ReadFile(archiveLine)
			if err != nil || !strings.Contains(string(md), "Old child research notes") {
				t.Fatal("invalid Markdown archive", err)
			}
			r.orderMu.Lock()
			notices := append([]provider.Message(nil), r.notifications...)
			r.orderMu.Unlock()
			if aside && len(notices) != 0 {
				t.Fatal("aside leaked parent notification", notices)
			}
			if !aside {
				if len(notices) != 1 || notices[0].EventSeq <= 0 {
					t.Fatal("missing committed child event", notices)
				}
				var event map[string]any
				if err := json.Unmarshal([]byte(notices[0].Content), &event); err != nil || event["type"] != "child_compacted" || event["summary"] != nil {
					t.Fatal("invalid child compact notification", event, err)
				}
			}
		})
	}
}

func TestFatalCompactionGuardSurvivesFailedInvalidationWrite(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Inspectable context")
	_, err := r.Store.DB.Exec(`CREATE TRIGGER fail_invalidation BEFORE UPDATE OF metadata_json ON sessions BEGIN SELECT RAISE(ABORT,'fixture invalidation write failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	err = r.compactionFailure(r.Current(), errors.New("invalid summary"))
	if err == nil || !strings.Contains(err.Error(), "persist unusable context") {
		t.Fatal("missing persistence failure", err)
	}
	if r.checkContext() == nil {
		t.Fatal("failed invalidation write reopened inference")
	}
	if _, err := r.Store.Transcript(r.Current(), 0); err != nil {
		t.Fatal("history became uninspectable", err)
	}
}

func TestCanceledChildCompactionPreservesCursorAndPublishesNothing(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Parent context")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cursor := contextCursor{project: "unchanged"}
	result, next, err := r.compactChild(ctx, childTask{actor: "main/child", tools: r.Tools}, nil, cursor)
	if !errors.Is(err, context.Canceled) || result != nil || next.project != cursor.project {
		t.Fatal("canceled child changed context", result, next, err)
	}
	r.orderMu.Lock()
	n := len(r.notifications)
	r.orderMu.Unlock()
	if n != 0 || r.checkContext() != nil {
		t.Fatal("canceled child invalidated parent or published event")
	}
}
