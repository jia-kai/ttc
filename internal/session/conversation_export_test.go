package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ttc/internal/llm"
	"ttc/internal/scratch"
	"ttc/internal/tool"
)

func TestConversationExportValidation(t *testing.T) {
	for _, args := range []conversationExportArgs{
		{}, {Path: "x", Format: "xml"}, {Path: "x", Scope: "latest"},
	} {
		if err := validateConversationExport(args); err == nil {
			t.Fatalf("accepted %+v", args)
		}
	}
	for _, args := range []conversationExportArgs{
		{Path: "x"}, {Path: "x", Format: "jsonl", Scope: "before_compaction"},
	} {
		if err := validateConversationExport(args); err != nil {
			t.Fatalf("rejected %+v: %v", args, err)
		}
	}
}

func exportTestPath(t *testing.T) string {
	t.Helper()
	root, err := scratch.Verify()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "conversation-export-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "export")
}

func TestConversationExportFormatsAndActor(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "main-only-marker")
	actor := "main/test-child"
	if _, err := r.Store.Append(r.Current(), "", actor, "message", "user", true, llm.Message{Type: llm.TypeUserInput, Content: "child-only-marker"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.Append(r.Current(), "", "main/other-child", "message", "user", true, llm.Message{Type: llm.TypeUserInput, Content: "sibling-secret-marker"}); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"", "jsonl"} {
		path := exportTestPath(t)
		value, err := r.exportConversation(context.Background(), tool.Execution{Actor: actor, SessionID: "ignored-stale-session"}, conversationExportArgs{Path: path, Format: format})
		if err != nil {
			t.Fatal(err)
		}
		result, ok := value.(ConversationExportResult)
		if !ok {
			t.Fatalf("result type %T", value)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if result.Bytes != int64(len(data)) || result.Path != path || result.Actor != actor || result.SourceSession != r.Current() || result.SourceTip == 0 || result.Scope != "full" {
			t.Fatalf("metadata=%+v bytes=%d", result, len(data))
		}
		wantFormat := format
		if wantFormat == "" {
			wantFormat = "markdown"
		}
		if result.Format != wantFormat {
			t.Fatalf("format=%q", result.Format)
		}
		if !strings.Contains(string(data), "child-only-marker") || strings.Contains(string(data), "main-only-marker") || strings.Contains(string(data), "sibling-secret-marker") {
			t.Fatalf("actor isolation failed: %s", data)
		}
		if format == "jsonl" {
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if !json.Valid([]byte(line)) {
					t.Fatalf("invalid JSONL: %q", line)
				}
			}
		}
		if _, err := r.exportConversation(context.Background(), tool.Execution{Actor: actor}, conversationExportArgs{Path: path}); err == nil {
			t.Fatal("overwrote existing export")
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(data) {
			t.Fatalf("existing export changed: %v", err)
		}
	}
}

func TestConversationExportToolCodec(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "fixture")
	for _, args := range []string{
		`{}`, `{"path":"x","format":"xml"}`, `{"path":"x","scope":"latest"}`,
		`{"path":"x","actor":"main"}`, `{"path":"x","session":"other"}`,
	} {
		record := r.Tools.Invoke(context.Background(), tool.Execution{Actor: "main"}, "conversation_export", []byte(args))
		var result struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(record.Result, &result); err != nil {
			t.Fatal(err)
		}
		if result.OK {
			t.Fatalf("accepted invalid args: %s", args)
		}
	}
}

func TestConversationExportCancelledAndMissingActor(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "fixture")
	path := exportTestPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.exportConversation(ctx, tool.Execution{Actor: "main"}, conversationExportArgs{Path: path}); err == nil {
		t.Fatal("ignored cancellation")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cancelled export created: %v", err)
	}
	if _, err := r.exportConversation(context.Background(), tool.Execution{}, conversationExportArgs{Path: path}); err == nil {
		t.Fatal("accepted missing actor")
	}
}

func TestConversationExportBeforeCompactionCutAndCleanup(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "main-evidence")
	actor := "main/export-child"
	appendChild := func(content string) int64 {
		t.Helper()
		id, err := r.Store.Append(r.Current(), "", actor, "message", "user", true, llm.Message{Type: llm.TypeUserInput, Content: content})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	cut := appendChild("earlier-assignment-marker")
	missingPath := exportTestPath(t)
	if _, err := r.exportConversation(context.Background(), tool.Execution{Actor: actor}, conversationExportArgs{Path: missingPath, Scope: "before_compaction"}); err == nil {
		t.Fatal("accepted before_compaction without a cut")
	}
	if _, err := os.Lstat(missingPath); !os.IsNotExist(err) {
		t.Fatalf("scope failure left output: %v", err)
	}
	if err := os.WriteFile(missingPath, []byte("preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.exportConversation(context.Background(), tool.Execution{Actor: actor}, conversationExportArgs{Path: missingPath, Scope: "before_compaction"}); !os.IsExist(err) {
		t.Fatalf("did history scope work before rejecting existing output: %v", err)
	}
	if err := r.Store.RecordActorCompaction(r.Current(), "", actor, r.Current(), cut); err != nil {
		t.Fatal(err)
	}
	appendChild("later-assignment-marker")
	for _, scope := range []string{"full", "before_compaction"} {
		path := exportTestPath(t)
		value, err := r.exportConversation(context.Background(), tool.Execution{Actor: actor}, conversationExportArgs{Path: path, Scope: scope})
		if err != nil {
			t.Fatal(err)
		}
		result := value.(ConversationExportResult)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "earlier-assignment-marker") {
			t.Fatalf("lost earlier assignment: %s", data)
		}
		if strings.Contains(string(data), "later-assignment-marker") != (scope == "full") {
			t.Fatalf("wrong scope %s: %s", scope, data)
		}
		if scope == "before_compaction" && (result.SourceTip != cut || result.SourceSession != r.Current()) {
			t.Fatalf("unresolved metadata: %+v", result)
		}
	}
}

func TestConversationExportAcrossMainContinuation(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "original-main-marker")
	actor := "main/retained-child"
	if _, err := r.Store.Append(r.Current(), "", actor, "message", "user", true, llm.Message{Type: llm.TypeUserInput, Content: "earlier-child-marker"}); err != nil {
		t.Fatal(err)
	}
	original, err := r.Store.Session(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	continued, err := r.Store.Continue(original.ID, "compaction-summary-marker", original.EntryTip, original.EntryTip+1, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.current = continued.ID
	r.mu.Unlock()
	for _, pair := range [][2]string{{"main", "later-main-marker"}, {actor, "later-child-marker"}} {
		if _, err := r.Store.Append(continued.ID, "", pair[0], "message", "user", true, llm.Message{Type: llm.TypeUserInput, Content: pair[1]}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		actor, scope     string
		include, exclude []string
	}{
		{"main", "before_compaction", []string{"original-main-marker"}, []string{"later-main-marker", "earlier-child-marker", "compaction-summary-marker"}},
		{actor, "full", []string{"earlier-child-marker", "later-child-marker"}, []string{"original-main-marker", "later-main-marker"}},
	} {
		path := exportTestPath(t)
		value, err := r.exportConversation(context.Background(), tool.Execution{Actor: tc.actor}, conversationExportArgs{Path: path, Scope: tc.scope})
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, marker := range tc.include {
			if !strings.Contains(string(data), marker) {
				t.Fatalf("lost %q: %s", marker, data)
			}
		}
		for _, marker := range tc.exclude {
			if strings.Contains(string(data), marker) {
				t.Fatalf("leaked %q: %s", marker, data)
			}
		}
		if tc.scope == "before_compaction" {
			result := value.(ConversationExportResult)
			if result.SourceSession != original.ID || result.SourceTip != original.EntryTip {
				t.Fatalf("wrong source metadata: %+v", result)
			}
		}
	}
}

func TestConversationExportAsideFreezesMainPrefix(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	path := exportTestPath(t)
	firstMain, advanceMain, secondMain := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var mainCalls, asideCalls atomic.Int32
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		finish := func() error { return emit(llm.StreamEvent{Kind: "completed"}) }
		if !strings.Contains(req.ConversationID, "/btw_") {
			if mainCalls.Add(1) == 1 {
				close(firstMain)
				select {
				case <-advanceMain:
				case <-ctx.Done():
					return ctx.Err()
				}
				if err := emit(llm.StreamEvent{Kind: "text", Text: "later-main-output-marker"}); err != nil {
					return err
				}
				call := llm.ToolCall{ID: "main-list", Name: "job_list", Arguments: []byte(`{}`)}
				if err := emit(llm.StreamEvent{Kind: "call", Call: &call}); err != nil {
					return err
				}
				return finish()
			}
			close(secondMain)
			<-ctx.Done()
			return ctx.Err()
		}
		if asideCalls.Add(1) == 1 {
			select {
			case <-secondMain:
			case <-ctx.Done():
				return ctx.Err()
			}
			args, _ := json.Marshal(conversationExportArgs{Path: path})
			call := llm.ToolCall{ID: "aside-export", Name: "conversation_export", Arguments: args}
			if err := emit(llm.StreamEvent{Kind: "call", Call: &call}); err != nil {
				return err
			}
			return finish()
		}
		if err := emit(llm.StreamEvent{Kind: "text", Text: "aside done"}); err != nil {
			return err
		}
		return finish()
	}}
	done := make(chan error, 1)
	go func() {
		message := llm.Message{Type: llm.TypeUserInput, Content: "frozen-main-prefix-marker"}
		done <- r.Run(&message)
	}()
	t.Cleanup(func() {
		r.Interrupt()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("main interruption=%v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("main did not stop")
		}
	})
	select {
	case <-firstMain:
	case <-time.After(3 * time.Second):
		t.Fatal("main did not start")
	}
	id, err := r.StartBTW("aside-own-question-marker")
	if err != nil {
		t.Fatal(err)
	}
	close(advanceMain)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	job, err := r.Jobs.Wait(ctx, "main", id, nil)
	if err != nil || job.Status != "completed" {
		t.Fatalf("aside job=%+v err=%v", job, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "frozen-main-prefix-marker") || !strings.Contains(string(data), "aside-own-question-marker") || strings.Contains(string(data), "later-main-output-marker") {
		t.Fatalf("wrong frozen aside export: %s", data)
	}
	select {
	case err := <-done:
		t.Fatalf("main ended during aside: %v", err)
	default:
	}
}
