package tui

import (
	"fmt"
	"strings"
	"testing"

	"scicode/internal/history"
)

func TestPromptRecallRestoresDraftWithoutSubmitting(t *testing.T) {
	var h promptHistory
	if got := h.move("draft", -1); got != "draft" {
		t.Fatal(got)
	}
	h.add("first")
	h.add("/model")
	h.add("/model")
	if len(h.entries) != 2 {
		t.Fatal("consecutive duplicate recalled twice")
	}
	for _, step := range []struct {
		draft     string
		direction int
		want      string
	}{{"unfinished", -1, "/model"}, {"/model", -1, "first"}, {"first", -1, "first"}, {"first", 1, "/model"}, {"/model", 1, "unfinished"}, {"unfinished", 1, "unfinished"}} {
		if got := h.move(step.draft, step.direction); got != step.want {
			t.Fatal(got, step.want)
		}
	}
	h.add("next")
	if got := h.move("new draft", -1); got != "next" {
		t.Fatal(got)
	}
}

func TestPromptRecallBoundsCurrentRunHistory(t *testing.T) {
	var h promptHistory
	for i := 0; i < history.MaxPromptHistoryEntries+5; i++ {
		h.add(fmt.Sprintf("prompt %d", i))
	}
	if len(h.entries) != history.MaxPromptHistoryEntries || h.entries[0] != "prompt 5" {
		t.Fatal("prompt count was not bounded")
	}
	first, second := strings.Repeat("a", 5<<20), strings.Repeat("b", 5<<20)
	h.add(first)
	h.add(second)
	if len(h.entries) != 1 || h.entries[0] != second || h.bytes != len(second) {
		t.Fatal("text budget did not evict older prompts")
	}
	h.add(strings.Repeat("x", history.MaxPromptHistoryBytes+1))
	if len(h.entries) != 1 || h.entries[0] != second {
		t.Fatal("oversized prompt entered recall")
	}
	if got := h.move("draft", -1); got != second {
		t.Fatal("eviction broke recall")
	}
	if got := h.move(second, 1); got != "draft" {
		t.Fatal("eviction broke draft restoration")
	}
}
