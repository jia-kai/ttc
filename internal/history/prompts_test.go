package history

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/provider"
)

func TestPromptHistoryAcrossSessionsAndRestart(t *testing.T) {
	s, first, turn, _ := historyFixture(t)
	appendMessage := func(actor, kind string, visible bool, m provider.Message) int64 {
		t.Helper()
		id, err := s.Append(first.ID, turn, actor, kind, m.Role, visible, m)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	appendMessage("main", "message", true, provider.Message{Role: "assistant", Content: "assistant text"})
	appendMessage("main", "message", true, provider.Message{Role: "user", Content: "runtime notice", Runtime: true})
	appendMessage("main/child", "message", true, provider.Message{Role: "user", Content: "child instruction"})
	appendMessage("main", "summary", true, provider.Message{Role: "user", Content: "compaction summary"})
	appendMessage("main", "message", false, provider.Message{Role: "user", Content: "internal naming input"})
	copyID := appendMessage("main", "message", true, provider.Message{Role: "user", Content: "hello"})
	var original int64
	if err := s.DB.QueryRow("SELECT min(id) FROM entries WHERE session_id=?", first.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE entries SET source_id=? WHERE id=?", original, copyID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	steer, err := s.BeginTurn(first.ID, "steer", first.Model)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Append(first.ID, steer, "main", "message", "user", true, provider.Message{Role: "user", Content: "Steering α\nsecond line"}); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishTurn(steer, "completed"); err != nil {
		t.Fatal(err)
	}
	second, next := startHistorySession(t, s, t.TempDir(), first.Model, "newer")
	authored := "look @fixture.txt"
	if _, err = s.Append(second.ID, next, "main", "message", "user", true, provider.Message{Role: "user", Content: "expanded private attachment", UserText: &authored}); err != nil {
		t.Fatal(err)
	}
	empty := ""
	if _, err = s.Append(second.ID, next, "main", "message", "user", true, provider.Message{Role: "user", Content: "attachment only", UserText: &empty}); err != nil {
		t.Fatal(err)
	}
	root := s.Root
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.PromptHistory(context.Background())
	want := []string{"hello", "Steering α\nsecond line", "newer", authored}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal(got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.PromptHistory(ctx); err == nil {
		t.Fatal("canceled query succeeded")
	}
}

func TestPromptHistoryNewestCountAndByteBudget(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	for i := 0; i < MaxPromptHistoryEntries+5; i++ {
		if _, err := s.Append(v.ID, turn, "main", "message", "user", true, provider.Message{Role: "user", Content: fmt.Sprintf("prompt %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.PromptHistory(context.Background())
	if err != nil || len(got) != MaxPromptHistoryEntries || got[0] != "prompt 5" || got[len(got)-1] != "prompt 1004" {
		t.Fatal(len(got), err)
	}
	big := strings.Repeat("界", (5<<20)/3)
	for range 2 {
		if _, err = s.Append(v.ID, turn, "main", "message", "user", true, provider.Message{Role: "user", Content: big}); err != nil {
			t.Fatal(err)
		}
	}
	got, err = s.PromptHistory(context.Background())
	if err != nil || len(got) != 1 || got[0] != big {
		t.Fatal("byte budget truncated or retained excess text", len(got), err)
	}
}
