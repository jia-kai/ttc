package session

import (
	"context"
	"strings"
	"sync"
	"testing"

	"scicode/internal/provider"
)

func TestRequestIdentityAcrossToolBoundariesChildrenTurnsAndSessionChanges(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	script := &provider.Script{Responses: []provider.ScriptResponse{
		{Calls: []provider.ToolCall{{ID: "child", Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"inspect","label":"inspect files"}`)}}},
		{Calls: []provider.ToolCall{{ID: "glob", Name: "glob", Arguments: []byte(`{"pattern":"*.txt"}`)}}},
		{Text: "Child done"}, {Text: "Parent done"},
		{Text: "Second turn"}, {Text: "New session"}, {Text: "Loaded session"},
	}}
	var mu sync.Mutex
	var ids []string
	r.Provider = &childProvider{stream: func(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
		mu.Lock()
		ids = append(ids, req.ConversationID)
		mu.Unlock()
		return script.Stream(ctx, req, emit)
	}}
	original := r.Current()
	for _, prompt := range []string{"Start child", "Another turn"} {
		if err := r.Run(&provider.Message{Role: "user", Content: prompt}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Command("/new"); err != nil {
		t.Fatal(err)
	}
	next := r.Current()
	if err := r.Run(&provider.Message{Role: "user", Content: "New"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Command("/load " + original); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(&provider.Message{Role: "user", Content: "Continue"}); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 7 || ids[0] != original || ids[3] != original || ids[4] != original || ids[5] != next || ids[6] != original || next == original {
		t.Fatal("incorrect main conversation identities", ids)
	}
	if ids[1] != ids[2] || !strings.HasPrefix(ids[1], "main/child_") || ids[1] == original || ids[1] == next {
		t.Fatal("incorrect child identity", ids)
	}
}
